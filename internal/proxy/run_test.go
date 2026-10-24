package proxy

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// proxyHome is a fresh HOME for a subprocess proxy, with its proxy
// directory (what process.ProxyDir creates) and a config for apps.
func proxyHome(t *testing.T, apps ...App) (home, dir string, s Settings) {
	t.Helper()
	home = shortDir(t)
	dir = filepath.Join(home, ".shinyctl", "proxy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s = SettingsFor(dir)
	cfg, err := Render(s, apps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	return home, dir, s
}

func TestRun_SecondInstanceRefused(t *testing.T) {
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up") }))
	app := upstreamApp("a", freePort(t), up)
	home, _, s := proxyHome(t, app)
	admin := Admin{Socket: s.AdminSocket}

	first := startSubproxyAt(t, s.ConfigPath, home)
	first.waitAnswering(t, admin)

	second := startSubproxyAt(t, s.ConfigPath, home)
	select {
	case <-second.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the second proxy did not exit within 2s")
	}
	if second.cmd.ProcessState.ExitCode() != 1 {
		t.Errorf("second proxy exit code = %d, want 1", second.cmd.ProcessState.ExitCode())
	}
	if want := fmt.Sprintf("already running (pid %d)", first.pid()); !strings.Contains(second.stderr.String(), want) {
		t.Errorf("second proxy stderr = %q, want it to contain %q", second.stderr.String(), want)
	}

	if err := admin.Ping(); err != nil {
		t.Errorf("the first proxy stopped answering: %v", err)
	}
	if status, body, _ := get(t, appURL(app)); status != 200 || body != "up" {
		t.Errorf("the first proxy answers %d %q, want 200 up", status, body)
	}
}

func TestRun_SIGTERMExitsAndReleasesLock(t *testing.T) {
	home, _, s := proxyHome(t)
	admin := Admin{Socket: s.AdminSocket}

	p := startSubproxyAt(t, s.ConfigPath, home)
	p.waitAnswering(t, admin)
	if !lockHeld(LockPath(s.ConfigPath)) {
		t.Fatal("the running proxy does not hold its lock")
	}
	info, err := readLock(LockPath(s.ConfigPath))
	if err != nil || info.PID != p.pid() || info.Build == "" {
		t.Errorf("lock = %+v, %v, want pid %d and a build", info, err, p.pid())
	}

	p.signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(6 * time.Second):
		t.Fatal("the proxy did not exit within 6s of SIGTERM")
	}
	if p.err != nil {
		t.Errorf("exit = %v, want a clean exit", p.err)
	}
	if lockHeld(LockPath(s.ConfigPath)) {
		t.Error("the lock is still held after the proxy exited")
	}
}

func TestRun_FallsBackWhenConfigFails(t *testing.T) {
	taken := freePort(t)
	hold(t, taken)
	home, _, s := proxyHome(t, upstreamApp("a", taken, "127.0.0.1:20000"))
	admin := Admin{Socket: s.AdminSocket}

	p := startSubproxyAt(t, s.ConfigPath, home)
	p.waitAnswering(t, admin)

	cfg, err := admin.Config()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), `"apps"`) {
		t.Errorf("the fallback config has apps:\n%s", cfg)
	}
	if !strings.Contains(p.stderr.String(), "serving no apps") {
		t.Errorf("stderr = %q, want it to say it is serving no apps", p.stderr.String())
	}
}

// TestRun_FallsBackWhenConfigIsUnusable: a config.json that isn't JSON, or is
// missing, still
// gives a load balancer that answers on its own socket and serves no apps,
// and its log does not name the engine.
func TestRun_FallsBackWhenConfigIsUnusable(t *testing.T) {
	cases := map[string]func(t *testing.T) []byte{
		"invalid JSON": func(t *testing.T) []byte { return []byte("{ this is not json") },
		"missing file": nil,
	}

	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			home, _, s := proxyHome(t)
			if mk == nil {
				os.Remove(s.ConfigPath)
			} else if err := os.WriteFile(s.ConfigPath, mk(t), 0600); err != nil {
				t.Fatal(err)
			}

			p := startSubproxyAt(t, s.ConfigPath, home)
			p.waitAnswering(t, Admin{Socket: s.AdminSocket})

			cfg, err := (Admin{Socket: s.AdminSocket}).Config()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(cfg), `"apps"`) {
				t.Errorf("the fallback config has apps:\n%s", cfg)
			}
			if !strings.Contains(p.stderr.String(), "serving no apps") {
				t.Errorf("stderr = %q, want it to say it is serving no apps", p.stderr.String())
			}
			if productName.MatchString(p.stderr.String()) {
				t.Errorf("stderr names the software:\n%s", p.stderr.String())
			}
		})
	}
}

// TestRun_ConfigCannotMoveTheControlSocket: a config.json that loads fine but
// names another existing directory for its control socket and storage is
// served from this directory's own, so the supervisor still reaches it and
// the other directory is left alone.
func TestRun_ConfigCannotMoveTheControlSocket(t *testing.T) {
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up") }))
	app := upstreamApp("a", freePort(t), up)
	home, _, s := proxyHome(t, app)

	other := SettingsFor(shortDir(t))
	cfg, err := Render(other, []App{app})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}

	p := startSubproxyAt(t, s.ConfigPath, home)
	p.waitAnswering(t, Admin{Socket: s.AdminSocket})

	if status, body, _ := get(t, appURL(app)); status != 200 || body != "up" {
		t.Errorf("the apps of the file answer %d %q, want 200 up", status, body)
	}
	if _, err := os.Stat(other.AdminSocket); err == nil {
		t.Errorf("a control socket was created in the other directory %s", other.AdminSocket)
	}
}

// TestRun_WritesNothingOutsideItsDir: the proxy leaves nothing in HOME apart
// from its own directory, so it can't scatter state into the user's data
// directories.
func TestRun_WritesNothingOutsideItsDir(t *testing.T) {
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up") }))
	app := upstreamApp("a", freePort(t), up)
	home, dir, s := proxyHome(t, app)
	admin := Admin{Socket: s.AdminSocket}

	p := startSubproxyAt(t, s.ConfigPath, home)
	p.waitAnswering(t, admin)
	get(t, appURL(app))
	p.signal(syscall.SIGTERM)
	<-p.exited

	filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Error(err)
			return nil
		}
		if d.IsDir() || strings.HasPrefix(path, dir+string(filepath.Separator)) {
			return nil
		}
		t.Errorf("the proxy left %s outside %s", path, dir)
		return nil
	})
}

// TestWithOwnSettings: whatever a file says about the control socket, logging
// and storage, the proxy's own directory's settings are used, and the file's
// apps are kept.
func TestWithOwnSettings(t *testing.T) {
	app := upstreamApp("a", 8080, "127.0.0.1:20000")
	file, err := Render(SettingsFor("/somewhere/else/proxy"), []App{app})
	if err != nil {
		t.Fatal(err)
	}
	got, err := withOwnSettings(file, "/home/me/.shinyctl/proxy")
	if err != nil {
		t.Fatal(err)
	}

	want, err := Render(SettingsFor("/home/me/.shinyctl/proxy"), []App{app})
	if err != nil {
		t.Fatal(err)
	}
	gotV, _ := decodeConfig(got)
	wantV, _ := decodeConfig(want)
	if !reflect.DeepEqual(gotV, wantV) {
		t.Errorf("config = %s\nwant %s", got, want)
	}
}
