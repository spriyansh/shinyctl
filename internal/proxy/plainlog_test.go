package proxy

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPlainLine(t *testing.T) {
	cases := []struct {
		name  string
		entry string
		want  string
	}{
		{"a copy dies in the middle of a response",
			`{"level":"warn","ts":1.7e9,"logger":"http.handlers.reverse_proxy","msg":"aborting with incomplete response","upstream":"127.0.0.1:20001","duration":0.01,"request":{"remote_ip":"1.2.3.4","method":"GET","uri":"/path?x=1","proto":"HTTP/1.1"},"error":"unexpected EOF"}`,
			"proxy: the copy at 127.0.0.1:20001 stopped in the middle of a response (GET /path?x=1): unexpected EOF"},
		{"the same without a request or error",
			`{"level":"warn","logger":"http.handlers.reverse_proxy","msg":"aborting with incomplete response","upstream":"127.0.0.1:20001"}`,
			"proxy: the copy at 127.0.0.1:20001 stopped in the middle of a response"},
		{"a refused push is dropped",
			`{"level":"error","ts":1.7e9,"logger":"admin.api","msg":"request error","error":"bad request","status_code":400}`,
			""},
		{"a server error of the control socket is kept",
			`{"level":"error","logger":"admin.api","msg":"request error","error":"boom","status_code":500}`,
			"proxy: error: request error: boom"},
		{"anything else, with an upstream",
			`{"level":"error","logger":"http.log.error","msg":"reading from backend","error":"read tcp: connection reset","upstream":"127.0.0.1:20002"}`,
			"proxy: error: reading from backend: read tcp: connection reset (upstream 127.0.0.1:20002)"},
		{"anything else, with an address",
			`{"level":"warn","msg":"stopping listener","address":"tcp/0.0.0.0:8080"}`,
			"proxy: warn: stopping listener (address tcp/0.0.0.0:8080)"},
		{"the engine's note about its own log is dropped",
			`{"level":"info","ts":1.7e9,"msg":"redirected default logger","from":"stderr","to":"shinyctl_plain"}`, ""},
		{"not JSON", "  something went wrong \n", "proxy: something went wrong"},
		{"nothing at all", "   ", ""},
		{"the engine's name never shows",
			`{"level":"error","msg":"loading module","error":"module github.com/caddyserver/caddy/v2/modules/x: Caddy failed"}`,
			"proxy: error: loading module: module github.com/engineserver/engine/v2/modules/x: engine failed"},
	}
	for _, c := range cases {
		got := plainLine([]byte(c.entry))
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
		if strings.Contains(strings.ToLower(got), "caddy") {
			t.Errorf("%s: the engine is named in %q", c.name, got)
		}
	}
}

func TestRender_LogsArePlain(t *testing.T) {
	out, err := Render(settingsFor(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(out), `"output": "shinyctl_plain"`) + strings.Count(string(out), `"output":"shinyctl_plain"`); got != 2 {
		t.Errorf("rendered settings name the plain writer %d times, want 2 (both logs):\n%s", got, out)
	}
	if strings.Contains(string(out), `"stderr"`) {
		t.Errorf("a log still writes to stderr as the engine does:\n%s", out)
	}
}

func TestPlainCopy(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // the lines, without the timestamp the logger adds
	}{
		{"a line that is not JSON passes, with the engine's name replaced",
			"loading the Caddy module\n", "loading the engine module\n"},
		{"a JSON entry becomes a plain line",
			`{"level":"warn","msg":"something odd"}` + "\n", "proxy: warn: something odd\n"},
		{"the engine's own note is left out",
			`{"level":"info","msg":"redirected default logger","from":"stderr","to":"x"}` + "\n", ""},
		{"a last line without a newline is written",
			"first\nlast CADDY words", "first\nlast engine words\n"},
		{"a JSON entry without a newline at the end is made plain too",
			`{"level":"error","msg":"stopped"}`, "proxy: error: stopped\n"},
		{"empty lines are dropped", "\n\nkept\n\n", "kept\n"},
	}
	for _, c := range cases {
		var out bytes.Buffer
		plainCopy(strings.NewReader(c.in), &out)
		// Drop the "2026/10/28 10:00:00 " the logger puts before its lines.
		var got []string
		for _, l := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
			if len(l) > 20 && l[4] == '/' && l[7] == '/' && l[10] == ' ' && l[13] == ':' {
				l = l[20:]
			}
			got = append(got, l)
		}
		g := strings.Join(got, "\n")
		if g != "" {
			g += "\n"
		}
		if g != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, g, c.want)
		}
	}
}

// If load panics, standard error is put back, the reader has copied out what
// was written before, and the panic goes on.
func TestPlainStderr_PutsStandardErrorBackWhenLoadPanics(t *testing.T) {
	file := filepath.Join(t.TempDir(), "stderr")
	f, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	saved, err := unix.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(saved)
	if err := unix.Dup2(int(f.Fd()), 2); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			unix.Dup2(saved, 2)
		}
	}
	defer restore()

	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recovered %v, want the panic to go on", r)
			}
		}()
		plainStderr(func() error {
			fmt.Fprintln(os.Stderr, "written by Caddy before the panic")
			panic("boom")
		})
	}()
	// Standard error is the file again, not the pipe the reader was on.
	var atPanic, want unix.Stat_t
	if err := unix.Fstat(2, &atPanic); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(file, &want); err != nil {
		t.Fatal(err)
	}
	if atPanic.Ino != want.Ino || atPanic.Dev != want.Dev {
		t.Error("standard error was not put back when load panicked")
	}
	fmt.Fprintln(os.Stderr, "written after")
	restore()

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "written by engine before the panic\n") {
		t.Errorf("standard error = %q, want what was written before the panic, in plain words", data)
	}
	if !strings.Contains(string(data), "written after\n") {
		t.Errorf("standard error = %q, want what is written after to reach it directly", data)
	}
}
