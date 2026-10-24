/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
)

func TestLastLogLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api.log")
	if got := lastLogLine(path); got != "" {
		t.Errorf("a missing log: %q", got)
	}
	os.WriteFile(path, []byte("first\n2026/10/24 boom: it broke\n\n"), 0o644)
	if got := lastLogLine(path); got != ": 2026/10/24 boom: it broke" {
		t.Errorf("lastLogLine = %q", got)
	}
	os.WriteFile(path, nil, 0o644)
	if got := lastLogLine(path); got != "" {
		t.Errorf("an empty log: %q", got)
	}

	// Only the end of a big log is read.
	big := bytes.Repeat([]byte("a line that fills the log up\n"), 200000)
	os.WriteFile(path, append(big, "the last one\n"...), 0o644)
	if got := lastLogLine(path); got != ": the last one" {
		t.Errorf("lastLogLine of a big log = %q", got)
	}
}

// fakeDetacher is a detacher whose child answers after answerAfter polls
// (never when negative), or exits after exitAfter polls (never when
// negative).
func fakeDetacher(t *testing.T, running bool, answerAfter, exitAfter int) (*detacher, *bytes.Buffer, *atomic.Int32, *atomic.Int32) {
	return fakeDetacherPID(t, running, answerAfter, exitAfter, 4321)
}

// fakeDetacherPID is fakeDetacher with the pid the answering daemon names
// (4321 is the child's own).
func fakeDetacherPID(t *testing.T, running bool, answerAfter, exitAfter, answeringPID int) (*detacher, *bytes.Buffer, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var out bytes.Buffer
	var started, polls atomic.Int32
	exited := make(chan struct{})
	logPath := filepath.Join(t.TempDir(), "api.log")
	os.WriteFile(logPath, []byte("2026/10/24 shinyctl-api starting...\n2026/10/24 Error: the registry is corrupt\n"), 0o644)
	d := &detacher{
		socketPath:     "/x/api.sock",
		logPath:        logPath,
		alreadyRunning: func() bool { return running },
		healthy: func() (int, bool) {
			n := int(polls.Add(1))
			if exitAfter >= 0 && n > exitAfter {
				select {
				case <-exited:
				default:
					close(exited)
				}
			}
			return answeringPID, answerAfter >= 0 && n > answerAfter
		},
		start: func() (int, <-chan struct{}, error) {
			started.Add(1)
			return 4321, exited, nil
		},
		wait: 2 * time.Second,
		out:  &out,
	}
	return d, &out, &started, &polls
}

func TestDetacher_AnswersMeansStarted(t *testing.T) {
	d, out, started, polls := fakeDetacher(t, false, 3, -1) // answers on the 4th look
	if err := d.run(); err != nil {
		t.Fatal(err)
	}
	if started.Load() != 1 || polls.Load() < 4 {
		t.Errorf("started %d times, looked %d times: it must wait for the answer", started.Load(), polls.Load())
	}
	if want := "shinyctl-api started in background: pid 4321, logs at " + d.logPath + "\n"; out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
}

func TestDetacher_AlreadyRunningStartsNothing(t *testing.T) {
	d, out, started, _ := fakeDetacher(t, true, 0, -1)
	if err := d.run(); err != nil {
		t.Fatalf("already running is not an error: %v", err)
	}
	if started.Load() != 0 {
		t.Error("a second shinyctl-api was started")
	}
	if want := "shinyctl-api is already running (socket /x/api.sock)\n"; out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
}

func TestDetacher_ChildExitingFirstIsAnErrorWithTheLastLogLine(t *testing.T) {
	d, out, _, _ := fakeDetacher(t, false, -1, 2)
	err := d.run()
	if err == nil || !strings.Contains(err.Error(), "exited right after starting") || !strings.Contains(err.Error(), d.logPath) || !strings.Contains(err.Error(), "the registry is corrupt") {
		t.Errorf("err = %v, want the early exit with the log's last line", err)
	}
	if strings.Contains(out.String(), "started") {
		t.Errorf("it said %q", out.String())
	}
}

func TestDetacher_NoAnswerIsAnError(t *testing.T) {
	d, _, _, _ := fakeDetacher(t, false, -1, -1)
	d.wait = 200 * time.Millisecond
	err := d.run()
	if err == nil || !strings.Contains(err.Error(), "did not answer within 200ms") || !strings.Contains(err.Error(), "pid 4321") {
		t.Errorf("err = %v", err)
	}
}

func TestDetacher_StartFailureIsReturned(t *testing.T) {
	d, _, _, _ := fakeDetacher(t, false, 0, -1)
	d.start = func() (int, <-chan struct{}, error) { return 0, nil, errors.New("no such file") }
	if err := d.run(); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("err = %v", err)
	}
}

// api.Healthy is what the real detacher asks: a socket nobody answers on, or
// one that is only bound and not served, is not healthy.
func TestHealthy(t *testing.T) {
	sock := filepath.Join(shortTmp(t), "a.sock")
	if _, ok := api.Healthy(sock); ok {
		t.Error("a socket that does not exist is healthy")
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, ok := api.Healthy(sock); ok {
		t.Error("a socket that is bound and not served is healthy")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()
	if _, ok := api.Healthy(sock); !ok {
		t.Error("a served socket is not healthy")
	}
}

func shortTmp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hz")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// Two daemons started at once: the child lost the socket and has gone, and
// the other one answers. Its pid is not the child's.
func TestDetacher_AnotherDaemonAnswersAndTheChildIsGone(t *testing.T) {
	d, out, _, _ := fakeDetacherPID(t, false, 0, 0, 777)
	if err := d.run(); err != nil {
		t.Fatalf("already running is not an error: %v", err)
	}
	if strings.Contains(out.String(), "started") || strings.Contains(out.String(), "4321") {
		t.Errorf("output %q names the child that exited", out.String())
	}
	if want := "shinyctl-api is already running (pid 777, socket /x/api.sock)\n"; out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
}

// A shinyctl-api that does not name its process (an older one) answering
// after the child has gone is the same case, without a pid.
func TestDetacher_AnOlderDaemonAnswersAndTheChildIsGone(t *testing.T) {
	d, out, _, _ := fakeDetacherPID(t, false, 0, 0, 0)
	if err := d.run(); err != nil {
		t.Fatal(err)
	}
	if want := "shinyctl-api is already running (socket /x/api.sock)\n"; out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
}

// What the real detacher asks: a socket that accepts and says nothing, or
// answers 503, is not up yet.
func TestNewDetacher_HealthIsTheHealthzAnswer(t *testing.T) {
	sock := filepath.Join(shortTmp(t), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	d := newDetacher("/x/shinyctl", sock, filepath.Join(t.TempDir(), "api.log"))

	// Accepts, never answers. The goroutine gets its own listener: the test
	// uses another one below.
	go func(ln net.Listener) {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}(ln)
	if _, ok := d.healthy(); ok {
		t.Error("a socket that accepts and never answers is healthy")
	}
	if !d.alreadyRunning() {
		t.Error("alreadyRunning should be the bare socket")
	}
	ln.Close()

	// Answers 503.
	os.Remove(sock)
	ln503, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	status := atomic.Int32{}
	status.Store(503)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.PIDHeader, "555")
		w.WriteHeader(int(status.Load()))
	})}
	go srv.Serve(ln503)
	defer srv.Close()
	if _, ok := d.healthy(); ok {
		t.Error("a 503 is healthy")
	}
	status.Store(200)
	if pid, ok := d.healthy(); !ok || pid != 555 {
		t.Errorf("healthy = %d, %v; want 555 and true", pid, ok)
	}
}

func TestHandleHealthz_NamesItsProcess(t *testing.T) {
	rec := httptest.NewRecorder()
	handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Header().Get(api.PIDHeader) != strconv.Itoa(os.Getpid()) {
		t.Errorf("status %d, %s = %q; want 200 and this process's pid", rec.Code, api.PIDHeader, rec.Header().Get(api.PIDHeader))
	}
}
