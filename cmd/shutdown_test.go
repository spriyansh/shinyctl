/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
)

// shutdownFake is a shutdowner with no processes behind it. up is whether the
// socket answers: starting shinyctl-api sets it, and /shutdown clears it
// unless stays is set.
type shutdownFake struct {
	s           *shutdowner
	log         []string
	out, errOut bytes.Buffer
	up          bool
	stays       bool

	statusResp api.StatusResponse
	statusErr  error
	balancer   int
	apps       []string
	serviceOn  bool
	shutResp   api.ShutdownResponse
	shutErr    error
}

// readLogger notes in the call log when the answer is first read.
type readLogger struct {
	f *shutdownFake
	r io.Reader
	n int
}

func (l *readLogger) Read(p []byte) (int, error) {
	if l.n == 0 {
		l.f.log = append(l.f.log, "answer")
	}
	l.n++
	return l.r.Read(p)
}

func newShutdownFake(input string) *shutdownFake {
	f := &shutdownFake{}
	f.s = &shutdowner{
		isTerminal: func() bool { return true },
		in:         &readLogger{f: f, r: strings.NewReader(input)},
		dialable:   func() bool { return f.up },
		status: func() (api.StatusResponse, error) {
			f.log = append(f.log, "status")
			return f.statusResp, f.statusErr
		},
		leftovers: func() (int, []string, error) {
			f.log = append(f.log, "leftovers")
			return f.balancer, f.apps, nil
		},
		service: func() (bool, error) {
			f.log = append(f.log, "service")
			if f.serviceOn {
				f.up = false
			}
			return f.serviceOn, nil
		},
		start: func() error {
			f.log = append(f.log, "start")
			f.up = true
			return nil
		},
		shutdown: func() (api.ShutdownResponse, error) {
			f.log = append(f.log, "shutdown")
			if f.shutErr == nil && !f.stays {
				f.up = false
			}
			return f.shutResp, f.shutErr
		},
		wait:    time.Second,
		logPath: "/home/u/.shinyctl/api.log",
	}
	f.s.out, f.s.errOut = &f.out, &f.errOut
	return f
}

func (f *shutdownFake) called() string { return strings.Join(f.log, ",") }

func sessions(n int) *int { return &n }

// twoApps is a status with hello (2 sessions over two copies) and memory (none).
func twoApps() api.StatusResponse {
	return api.StatusResponse{Apps: []api.AppStatus{
		{Name: "hello", Copies: []api.CopyStatus{{Alive: true, Sessions: sessions(1)}, {Alive: true, Sessions: sessions(1)}}},
		{Name: "memory", Copies: []api.CopyStatus{{Alive: true, Sessions: sessions(0)}}},
	}}
}

const twoAppsPrompt = "shinyctl shutdown stops every app, the load balancer and shinyctl-api. Running now:\n" +
	"  hello   2 open sessions\n" +
	"  memory  no open sessions\n" +
	"They start again with shinyctl serve -d or any apply.\n" +
	"Stop everything? [y/N] "

func TestShutdown_NothingRunning(t *testing.T) {
	f := newShutdownFake("")
	if err := f.s.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.out.String(); got != "nothing is running\n" {
		t.Errorf("out = %q", got)
	}
	if f.errOut.Len() != 0 {
		t.Errorf("errOut = %q, want no prompt", f.errOut.String())
	}
	if want := "leftovers,service,leftovers"; f.called() != want {
		t.Errorf("calls = %s, want %s (nothing started)", f.called(), want)
	}
}

func TestShutdown_AsksThenStops(t *testing.T) {
	f := newShutdownFake("y\n")
	f.up = true
	f.statusResp = twoApps()
	f.shutResp = api.ShutdownResponse{PID: 4242, Apps: []string{"hello", "memory"}, Balancer: true}

	if err := f.s.run(); err != nil {
		t.Fatal(err)
	}
	if want := twoAppsPrompt + "stopping; an app busy in R can take 10 seconds to stop\n"; f.errOut.String() != want {
		t.Errorf("errOut = %q, want %q", f.errOut.String(), want)
	}
	want := "stopped the apps \"hello\" and \"memory\"\nstopped the load balancer\nstopped shinyctl-api (pid 4242)\n"
	if f.out.String() != want {
		t.Errorf("out = %q, want %q", f.out.String(), want)
	}
	if want := "status,answer,service,shutdown"; f.called() != want {
		t.Errorf("calls = %s, want %s", f.called(), want)
	}
}

func TestShutdown_OneAppAndANote(t *testing.T) {
	f := newShutdownFake("")
	f.s.yes = true
	f.up = true
	f.shutResp = api.ShutdownResponse{PID: 7, Apps: []string{"hello"},
		Notes: []string{`"memory" (recorded pid 123 is not running this app, so it was not signaled)`}}
	if err := f.s.run(); err != nil {
		t.Fatal(err)
	}
	want := "stopped the app \"hello\"\n\"memory\" (recorded pid 123 is not running this app, so it was not signaled)\nstopped shinyctl-api (pid 7)\n"
	if f.out.String() != want {
		t.Errorf("out = %q, want %q", f.out.String(), want)
	}
}

func TestShutdown_DeclineStopsNothing(t *testing.T) {
	for name, input := range map[string]string{"n": "n\n", "empty": "\n", "eof": "", "other": "maybe\n"} {
		t.Run(name, func(t *testing.T) {
			f := newShutdownFake(input)
			f.up = true
			f.statusResp = twoApps()
			if err := f.s.run(); err != nil {
				t.Fatal(err)
			}
			if got := f.out.String(); got != "nothing was stopped\n" {
				t.Errorf("out = %q", got)
			}
			if want := "status,answer"; f.called() != want {
				t.Errorf("calls = %s, want %s", f.called(), want)
			}
		})
	}
}

func TestShutdown_AnswerYesInAnyCase(t *testing.T) {
	for _, input := range []string{"y\n", "Y\n", "yes\n", " YES \n", "y"} {
		f := newShutdownFake(input)
		f.up = true
		f.statusResp = twoApps()
		if err := f.s.run(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(f.called(), "shutdown") {
			t.Errorf("%q did not go ahead: calls = %s", input, f.called())
		}
	}
}

func TestShutdown_YesSkipsThePrompt(t *testing.T) {
	f := newShutdownFake("")
	f.s.yes = true
	f.s.isTerminal = func() bool { return false }
	f.up = true
	f.statusResp = twoApps()
	if err := f.s.run(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.errOut.String(), "Stop everything?") {
		t.Errorf("errOut = %q, want no prompt", f.errOut.String())
	}
	if want := "status,service,shutdown"; f.called() != want {
		t.Errorf("calls = %s, want %s", f.called(), want)
	}
}

func TestShutdown_NoTerminalNeedsYes(t *testing.T) {
	f := newShutdownFake("y\n")
	f.s.isTerminal = func() bool { return false }
	f.up = true
	f.statusResp = twoApps()

	err := f.s.run()
	want := `shinyctl shutdown would stop running apps ("hello" and "memory") and has no terminal to ask; run shinyctl shutdown --yes to stop them without asking`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
	if f.called() != "status" || f.errOut.Len() != 0 {
		t.Errorf("calls = %s, errOut = %q, want only status", f.called(), f.errOut.String())
	}
}

func TestShutdown_NoPromptWhenNothingRuns(t *testing.T) {
	f := newShutdownFake("")
	f.s.isTerminal = func() bool { return false }
	f.up = true
	f.statusResp = api.StatusResponse{Apps: []api.AppStatus{
		{Name: "hello", Copies: []api.CopyStatus{{Alive: false}}},
		{Name: "old", Stopped: true, Copies: []api.CopyStatus{{Alive: true}}},
	}}
	f.shutResp = api.ShutdownResponse{PID: 9, Balancer: true}
	if err := f.s.run(); err != nil {
		t.Fatal(err)
	}
	want := "no app was running\nstopped the load balancer\nstopped shinyctl-api (pid 9)\n"
	if f.out.String() != want {
		t.Errorf("out = %q, want %q", f.out.String(), want)
	}
	if strings.Contains(f.errOut.String(), "Stop everything?") {
		t.Error("asked although nothing was running")
	}
}

func TestShutdown_StatusFails(t *testing.T) {
	f := newShutdownFake("y\n")
	f.up = true
	f.statusErr = errors.New("calling shinyctl-api: boom")
	err := f.s.run()
	if err == nil || err.Error() != "calling shinyctl-api: boom" {
		t.Errorf("err = %v", err)
	}
	if f.called() != "status" || f.errOut.Len() != 0 || f.out.Len() != 0 {
		t.Errorf("calls = %s, errOut = %q, out = %q", f.called(), f.errOut.String(), f.out.String())
	}
}

func TestShutdown_SessionsUnknown(t *testing.T) {
	f := newShutdownFake("n\n")
	f.up = true
	f.statusResp = api.StatusResponse{Apps: []api.AppStatus{
		{Name: "hello", Copies: []api.CopyStatus{{Alive: true}}},
		{Name: "memory", Copies: []api.CopyStatus{{Alive: true, Sessions: sessions(1)}, {Alive: true}}},
	}}
	if err := f.s.run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.errOut.String(), "\n  hello\n") {
		t.Errorf("prompt %q: hello should have its name only", f.errOut.String())
	}
	if !strings.Contains(f.errOut.String(), "  memory  1 open session\n") {
		t.Errorf("prompt %q: memory should say 1 open session", f.errOut.String())
	}
}

func TestShutdown_LinuxPromptSaysBoot(t *testing.T) {
	f := newShutdownFake("n\n")
	f.s.linux = true
	f.up = true
	f.statusResp = twoApps()
	f.s.run()
	if !strings.Contains(f.errOut.String(), "They start again with shinyctl serve -d, any apply, or at boot if the shinyctl service is enabled.\n") {
		t.Errorf("prompt = %q", f.errOut.String())
	}
}

func TestShutdown_DaemonDownWithLeftovers(t *testing.T) {
	f := newShutdownFake("y\n")
	f.balancer = 77
	f.apps = []string{"hello"}
	f.shutResp = api.ShutdownResponse{PID: 5, Apps: []string{"hello"}, Balancer: true}

	if err := f.s.run(); err != nil {
		t.Fatal(err)
	}
	if want := "shinyctl shutdown stops every app, the load balancer and shinyctl-api. Running now:\n  hello\nThey start"; !strings.HasPrefix(f.errOut.String(), want) {
		t.Errorf("errOut = %q, want a prompt with the name only", f.errOut.String())
	}
	want := "shinyctl-api is not running; starting it to stop what it left behind\nstopped the app \"hello\"\nstopped the load balancer\n"
	if f.out.String() != want {
		t.Errorf("out = %q, want %q (no pid line for a helper daemon)", f.out.String(), want)
	}
	if want := "leftovers,answer,service,leftovers,start,shutdown"; f.called() != want {
		t.Errorf("calls = %s, want %s", f.called(), want)
	}
}

func TestShutdown_OlderDaemon(t *testing.T) {
	f := newShutdownFake("")
	f.s.yes = true
	f.up = true
	f.shutErr = api.ErrNoShutdown
	err := f.s.run()
	want := `shinyctl-api is an older build without shutdown; stop it with pkill -u "$USER" -f '(^|/)shinyctl serve$' and run shinyctl shutdown again`
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}

func TestShutdown_RefusalIsReturnedAsItIs(t *testing.T) {
	f := newShutdownFake("")
	f.s.yes = true
	f.up = true
	f.shutErr = errors.New("shinyctl-api: it is already shutting down")
	if err := f.s.run(); err != f.shutErr {
		t.Errorf("err = %v, want the daemon's", err)
	}
}

func TestShutdown_DaemonDoesNotLeave(t *testing.T) {
	f := newShutdownFake("")
	f.s.yes = true
	f.s.wait = 100 * time.Millisecond
	f.up = true
	f.stays = true
	f.shutResp = api.ShutdownResponse{PID: 4242}
	err := f.s.run()
	want := "shinyctl-api (pid 4242) still answers after 100ms; see /home/u/.shinyctl/api.log"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}

func TestShutdown_Problems(t *testing.T) {
	f := newShutdownFake("")
	f.s.yes = true
	f.up = true
	f.shutResp = api.ShutdownResponse{PID: 1, Apps: []string{"hello"},
		Problems: []string{`"hello": stopping copy 2 (pid 4321): permission denied`, "the load balancer: boom"}}
	err := f.s.run()
	if err == nil || err.Error() != "some of it could not be stopped; see the warnings above" {
		t.Errorf("err = %v", err)
	}
	want := "warning: \"hello\": stopping copy 2 (pid 4321): permission denied\nwarning: the load balancer: boom\n"
	if !strings.HasSuffix(f.errOut.String(), want) {
		t.Errorf("errOut = %q, want it to end with the warnings", f.errOut.String())
	}
}

func TestListNames(t *testing.T) {
	for in, want := range map[string]string{
		"a":     `"a"`,
		"a,b":   `"a" and "b"`,
		"a,b,c": `"a", "b" and "c"`,
	} {
		if got := listNames(strings.Split(in, ",")); got != want {
			t.Errorf("listNames(%s) = %s, want %s", in, got, want)
		}
	}
}
