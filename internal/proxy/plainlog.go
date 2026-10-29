package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"golang.org/x/sys/unix"
)

// The load balancer's engine writes its log as one JSON object per line to
// standard error, which is api.log. The writer below, a log writer the
// rendered settings name, turns each entry into a line in the same form as
// the rest of api.log, or drops it when shinyctl-api already says the same in
// its own words.
//
// A Go runtime panic or a crash of the process still writes its trace to
// standard error as it is, naming source files. Nothing can rewrite what a
// dying process prints, so that stays.
func init() {
	caddy.RegisterModule(plainLogWriter{})
}

// plainLogWriterName is the writer's name in the rendered settings.
const plainLogWriterName = "shinyctl_plain"

type plainLogWriter struct{}

func (plainLogWriter) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "caddy.logging.writers." + plainLogWriterName,
		New: func() caddy.Module { return new(plainLogWriter) },
	}
}

func (plainLogWriter) String() string    { return plainLogWriterName }
func (plainLogWriter) WriterKey() string { return "shinyctl:plain" }

func (plainLogWriter) OpenWriter() (io.WriteCloser, error) {
	return plainLogSink{log.New(os.Stderr, "", log.LstdFlags)}, nil
}

// plainLogSink gets one encoded entry per Write (zap writes each entry
// whole), and tolerates several lines in one.
type plainLogSink struct{ out *log.Logger }

func (s plainLogSink) Write(p []byte) (int, error) {
	for _, entry := range bytes.Split(p, []byte("\n")) {
		if line := plainLine(entry); line != "" {
			s.out.Println(line)
		}
	}
	return len(p), nil
}

// Close does nothing: the sink writes to standard error, which stays open.
func (plainLogSink) Close() error { return nil }

var engineName = regexp.MustCompile(`(?i)caddy`)

// plainLine turns one engine log entry into a line for api.log, or "" to drop
// it. The engine's name is replaced in every line: error strings and module
// paths can carry it.
func plainLine(entry []byte) string {
	entry = bytes.TrimSpace(entry)
	if len(entry) == 0 {
		return ""
	}
	var e struct {
		Level      string `json:"level"`
		Logger     string `json:"logger"`
		Msg        string `json:"msg"`
		Error      string `json:"error"`
		Upstream   string `json:"upstream"`
		Address    string `json:"address"`
		StatusCode int    `json:"status_code"`
		Request    struct {
			Method string `json:"method"`
			URI    string `json:"uri"`
		} `json:"request"`
	}
	var line string
	if err := json.Unmarshal(entry, &e); err != nil || e.Msg == "" {
		line = "proxy: " + string(entry)
	} else {
		switch {
		case e.Msg == "redirected default logger":
			// The engine saying that its log now goes to the writer above.
			return ""
		case e.Logger == "admin.api" && e.StatusCode > 0 && e.StatusCode < 500:
			// A refused push: shinyctl-api logs its own failed pushes.
			return ""
		case e.Msg == "aborting with incomplete response" && e.Upstream != "":
			line = fmt.Sprintf("proxy: the copy at %s stopped in the middle of a response", e.Upstream)
			if e.Request.Method != "" || e.Request.URI != "" {
				line += fmt.Sprintf(" (%s)", strings.TrimSpace(e.Request.Method+" "+e.Request.URI))
			}
			if e.Error != "" {
				line += ": " + e.Error
			}
		default:
			line = fmt.Sprintf("proxy: %s: %s", e.Level, e.Msg)
			if e.Error != "" {
				line += ": " + e.Error
			}
			switch {
			case e.Upstream != "":
				line += fmt.Sprintf(" (upstream %s)", e.Upstream)
			case e.Address != "":
				line += fmt.Sprintf(" (address %s)", e.Address)
			}
		}
	}
	return engineName.ReplaceAllString(line, "engine")
}

// plainStderr runs load with the process's standard error read through
// plainCopy. The engine says "redirected default logger" in JSON, on the
// standard error it started with, when its first settings are loaded and
// the writer above takes over; this is the one moment its output can't go
// through the writer. Lines that are not JSON pass as they are, with the
// engine's name replaced.
//
// Standard error is put back by a deferred call, so it is also put back when
// load panics: the reader copies out everything written so far, and only then
// does the panic go on, so its trace is written to the real standard error.
func plainStderr(load func() error) error {
	r, w, err := os.Pipe()
	if err != nil {
		return load()
	}
	fd := int(os.Stderr.Fd())
	saved, err := unix.Dup(fd)
	if err != nil {
		r.Close()
		w.Close()
		return load()
	}
	out := os.NewFile(uintptr(saved), "stderr")
	if err := unix.Dup2(int(w.Fd()), fd); err != nil {
		r.Close()
		w.Close()
		out.Close()
		return load()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		plainCopy(r, out)
	}()
	defer func() {
		unix.Dup2(saved, fd) // standard error is itself again
		w.Close()            // the reader sees the end
		<-done
		r.Close()
		out.Close()
	}()
	return load()
}

// plainCopy reads the engine's output from r and writes it to out line by
// line: a JSON entry as plainLine says (or not at all), anything else as it
// is, with the engine's name replaced. A last line without a newline is
// written too.
func plainCopy(r io.Reader, out io.Writer) {
	logger := log.New(out, "", log.LstdFlags)
	line := func(b []byte) {
		b = bytes.TrimRight(b, "\r")
		switch {
		case len(b) == 0:
		case b[0] == '{':
			if l := plainLine(b); l != "" {
				logger.Println(l)
			}
		default:
			out.Write(append([]byte(engineName.ReplaceAllString(string(b), "engine")), '\n'))
		}
	}
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		for {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				break
			}
			line(buf[:i])
			buf = buf[i+1:]
		}
		if err != nil {
			line(buf)
			return
		}
	}
}
