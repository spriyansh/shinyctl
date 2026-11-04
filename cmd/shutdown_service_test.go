/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/process"
)

func TestServiceUnit(t *testing.T) {
	const (
		userV2    = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/shinyctl.service\n"
		userV1    = "1:name=systemd:/user.slice/user-1000.slice/user@1000.service/app.slice/shinyctl.service\n0::/\n"
		codeV2    = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/code-server.service\n"
		scopeV2   = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-gnome-terminal-1234.scope\n"
		systemV2  = "0::/system.slice/shinyctl.service\n"
		sessionV2 = "0::/user.slice/user-1000.slice/session-3.scope\n"
	)
	cases := []struct {
		name     string
		env      map[string]string
		cgroup   string
		wantUnit string
		wantUser bool
	}{
		{"exec pid is ours", map[string]string{"SYSTEMD_EXEC_PID": "42"}, userV2, "shinyctl.service", true},
		{"exec pid is another process", map[string]string{"SYSTEMD_EXEC_PID": "41"}, userV2, "", false},
		{"exec pid is not a number", map[string]string{"SYSTEMD_EXEC_PID": "x"}, userV2, "", false},
		{"exec pid is ours in a renamed unit", map[string]string{"SYSTEMD_EXEC_PID": "42"}, "0::/user.slice/user-1000.slice/user@1000.service/app.slice/r-apps.service\n", "r-apps.service", true},
		{"exec pid is ours and the cgroup is unreadable", map[string]string{"SYSTEMD_EXEC_PID": "42"}, "", "shinyctl.service", true},
		{"exec pid is ours in a system service", map[string]string{"SYSTEMD_EXEC_PID": "42"}, systemV2, "shinyctl.service", false},
		{"before 248, unified hierarchy", map[string]string{"INVOCATION_ID": "abc"}, userV2, "shinyctl.service", true},
		{"before 248, name=systemd hierarchy", map[string]string{"INVOCATION_ID": "abc"}, userV1, "shinyctl.service", true},
		{"before 248, a shell inside another service", map[string]string{"INVOCATION_ID": "abc"}, codeV2, "", false},
		{"before 248, a scope", map[string]string{"INVOCATION_ID": "abc"}, scopeV2, "", false},
		{"before 248, a login session", map[string]string{"INVOCATION_ID": "abc"}, sessionV2, "", false},
		{"before 248, no invocation id", nil, userV2, "", false},
		{"before 248, a system service is not guessed at", map[string]string{"INVOCATION_ID": "abc"}, systemV2, "", false},
		{"neither", nil, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			unit, user := serviceUnit(func(k string) string { return c.env[k] }, c.cgroup, 42)
			if unit != c.wantUnit || user != c.wantUser {
				t.Errorf("serviceUnit = %q, %v, want %q, %v", unit, user, c.wantUnit, c.wantUser)
			}
		})
	}
}

func TestSystemctlStop(t *testing.T) {
	if got := systemctlStop("shinyctl.service", true); got != "systemctl --user stop shinyctl.service" {
		t.Errorf("user: %s", got)
	}
	if got := systemctlStop("shinyctl.service", false); got != "systemctl stop shinyctl.service" {
		t.Errorf("system: %s", got)
	}
}

func TestHandleShutdown_RefusesUnderSystemd(t *testing.T) {
	useFakeRscript(t)
	spec, a := appliedApp(t, "shutdown-service")
	f := useShutdown(t)
	f.unit, f.userUnit = "shinyctl.service", true

	rec, _ := postShutdown(t, f)
	want := "it runs as the systemd service shinyctl.service, which would start it and every app again; run systemctl --user stop shinyctl.service, then shinyctl shutdown again\n"
	if rec.Code != http.StatusConflict || rec.Body.String() != want {
		t.Errorf("status = %d, body = %q, want 409 %q", rec.Code, rec.Body.String(), want)
	}
	if shuttingDown.Load() {
		t.Error("shuttingDown was set by a refusal")
	}
	if f.called() != "unit" {
		t.Errorf("calls = %s, want only unit", f.called())
	}
	if !process.IsAlive(spec, a) {
		t.Error("the app was stopped")
	}

	f.userUnit = false
	rec, _ = postShutdown(t, f)
	if !strings.Contains(rec.Body.String(), "run systemctl stop shinyctl.service,") {
		t.Errorf("system service: body = %q", rec.Body.String())
	}
}

func TestShutdown_ServiceAfterThePrompt(t *testing.T) {
	f := newShutdownFake("y\n")
	f.up = true
	f.serviceOn = true
	f.statusResp = twoApps()
	f.balancer = 9
	f.apps = []string{"hello", "memory"}
	f.shutResp = api.ShutdownResponse{PID: 5, Apps: []string{"hello", "memory"}, Balancer: true}

	if err := f.s.run(); err != nil {
		t.Fatal(err)
	}
	want := "stopped the shinyctl service (still enabled: systemctl --user disable shinyctl keeps it from starting at boot)\n" +
		"stopped the apps \"hello\" and \"memory\"\nstopped the load balancer\n"
	if f.out.String() != want {
		t.Errorf("out = %q, want %q (no \"starting it\" line, no pid line)", f.out.String(), want)
	}
	if want := "status,answer,service,leftovers,start,shutdown"; f.called() != want {
		t.Errorf("calls = %s, want %s: the question comes before the service is stopped", f.called(), want)
	}
}

func TestShutdown_ServiceStoppedAndNothingLeft(t *testing.T) {
	f := newShutdownFake("")
	f.s.yes = true
	f.up = true
	f.serviceOn = true
	if err := f.s.run(); err != nil {
		t.Fatal(err)
	}
	if want := "stopped the shinyctl service (still enabled: systemctl --user disable shinyctl keeps it from starting at boot)\n"; f.out.String() != want {
		t.Errorf("out = %q, want only %q", f.out.String(), want)
	}
	if strings.Contains(f.called(), "start") {
		t.Errorf("calls = %s: nothing was left to start a daemon for", f.called())
	}
}

func TestShutdown_ServiceFails(t *testing.T) {
	f := newShutdownFake("")
	f.s.yes = true
	f.up = true
	f.s.service = func() (bool, error) { return false, errors.New("boom") }
	err := f.s.run()
	if err == nil || err.Error() != "stopping the shinyctl service: boom" {
		t.Errorf("err = %v", err)
	}
	if f.called() != "status" {
		t.Errorf("calls = %s, want nothing stopped after the status", f.called())
	}
}

// serviceFake records the systemctl commands and answers is-active.
type serviceFake struct {
	env      serviceEnv
	ran      []string // each command, as "ENV args"
	isActive string
	isErr    error
	stopErr  error
}

func newServiceFake() *serviceFake {
	f := &serviceFake{}
	f.env = serviceEnv{
		goos:     "linux",
		uid:      1000,
		getenv:   func(string) string { return "" },
		exists:   func(string) bool { return false },
		lookPath: func(string) (string, error) { return "/usr/bin/systemctl", nil },
		run: func(env []string, args ...string) (string, error) {
			f.ran = append(f.ran, strings.TrimSpace(strings.Join(env, " ")+" "+strings.Join(args, " ")))
			if slices.Contains(args, "is-active") {
				return f.isActive, f.isErr
			}
			return "", f.stopErr
		},
	}
	return f
}

func TestStopService(t *testing.T) {
	exit3 := errors.New("exit status 3")
	cases := []struct {
		name      string
		set       func(f *serviceFake)
		want      bool
		wantErr   bool
		wantRan   []string
		errSubstr string
	}{
		{"active", func(f *serviceFake) { f.isActive = "active\n" }, true, false,
			[]string{"--user is-active shinyctl", "--user stop shinyctl"}, ""},
		{"activating", func(f *serviceFake) { f.isActive = "activating\n" }, true, false,
			[]string{"--user is-active shinyctl", "--user stop shinyctl"}, ""},
		{"deactivating", func(f *serviceFake) { f.isActive = "deactivating\n" }, true, false,
			[]string{"--user is-active shinyctl", "--user stop shinyctl"}, ""},
		{"reloading", func(f *serviceFake) { f.isActive = "reloading\n" }, true, false,
			[]string{"--user is-active shinyctl", "--user stop shinyctl"}, ""},
		{"inactive with a non-zero exit", func(f *serviceFake) { f.isActive, f.isErr = "inactive\n", exit3 }, false, false,
			[]string{"--user is-active shinyctl"}, ""},
		{"failed with a non-zero exit", func(f *serviceFake) { f.isActive, f.isErr = "failed\n", exit3 }, false, false,
			[]string{"--user is-active shinyctl"}, ""},
		{"active whatever the exit status says", func(f *serviceFake) { f.isActive, f.isErr = "active\n", exit3 }, true, false,
			[]string{"--user is-active shinyctl", "--user stop shinyctl"}, ""},
		{"no bus", func(f *serviceFake) {
			f.isActive, f.isErr = "", errors.New("Failed to connect to bus: No medium found")
		}, false, false, []string{"--user is-active shinyctl"}, ""},
		{"stop fails", func(f *serviceFake) { f.isActive, f.stopErr = "active\n", errors.New("timed out") }, false, true,
			[]string{"--user is-active shinyctl", "--user stop shinyctl"}, "timed out"},
		{"not linux", func(f *serviceFake) { f.env.goos = "darwin"; f.isActive = "active\n" }, false, false, nil, ""},
		{"no systemctl", func(f *serviceFake) {
			f.env.lookPath = func(string) (string, error) { return "", errors.New("not found") }
			f.isActive = "active\n"
		}, false, false, nil, ""},
		{"no runtime dir set, bus there", func(f *serviceFake) {
			f.env.exists = func(p string) bool { return p == "/run/user/1000/bus" }
			f.isActive = "active\n"
		}, true, false,
			[]string{"XDG_RUNTIME_DIR=/run/user/1000 --user is-active shinyctl", "XDG_RUNTIME_DIR=/run/user/1000 --user stop shinyctl"}, ""},
		{"no runtime dir set, no bus", func(f *serviceFake) { f.isActive = "inactive\n" }, false, false,
			[]string{"--user is-active shinyctl"}, ""},
		{"runtime dir set", func(f *serviceFake) {
			f.env.getenv = func(k string) string {
				if k == "XDG_RUNTIME_DIR" {
					return "/run/user/1000"
				}
				return ""
			}
			f.env.exists = func(string) bool { return true }
			f.isActive = "active\n"
		}, true, false, []string{"--user is-active shinyctl", "--user stop shinyctl"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newServiceFake()
			c.set(f)
			got, err := stopService(f.env)
			if got != c.want || (err != nil) != c.wantErr {
				t.Errorf("stopService = %v, %v, want %v, error %v", got, err, c.want, c.wantErr)
			}
			if c.errSubstr != "" && (err == nil || !strings.Contains(err.Error(), c.errSubstr)) {
				t.Errorf("err = %v, want it to say %q", err, c.errSubstr)
			}
			if fmt.Sprint(f.ran) != fmt.Sprint(c.wantRan) {
				t.Errorf("ran %q, want %q", f.ran, c.wantRan)
			}
		})
	}
}
