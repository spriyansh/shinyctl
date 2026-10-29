package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/proxy/lb"
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
	s = settingsFor(dir)
	cfg, err := Render(s, apps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	// The copies this process's table holds, for the proxy to read at start.
	copies, err := json.Marshal(lb.Current())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.CopiesPath, copies, 0600); err != nil {
		t.Fatal(err)
	}
	return home, dir, s
}

func TestRun_SecondInstanceRefused(t *testing.T) {
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up") }))
	app := servedApp(t, "a", freePort(t), up)
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
	home, _, s := proxyHome(t, plainApp("a", taken))
	admin := Admin{Socket: s.AdminSocket}

	p := startSubproxyAt(t, s.ConfigPath, home)
	p.waitAnswering(t, admin)
	// The control socket of the load that failed answers for a moment before
	// it is torn down; the fallback is up once the log says so.
	waitFor(t, "the fallback to be up", 10*time.Second, func() bool {
		return strings.Contains(p.stderr.String(), "serving no apps")
	})

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
	app := servedApp(t, "a", freePort(t), up)
	home, _, s := proxyHome(t, app)

	other := settingsFor(shortDir(t))
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
	app := servedApp(t, "a", freePort(t), up)
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
	app := plainApp("a", 8080)
	file, err := Render(settingsFor("/somewhere/else/proxy"), []App{app})
	if err != nil {
		t.Fatal(err)
	}
	got, err := withOwnSettings(file, "/home/me/.shinyctl/proxy")
	if err != nil {
		t.Fatal(err)
	}

	want, err := Render(settingsFor("/home/me/.shinyctl/proxy"), []App{app})
	if err != nil {
		t.Fatal(err)
	}
	gotV, _ := decodeConfig(got)
	wantV, _ := decodeConfig(want)
	if !reflect.DeepEqual(gotV, wantV) {
		t.Errorf("config = %s\nwant %s", got, want)
	}
}

// TestRun_ReadsCopiesFile: a proxy that starts on its own serves from
// copies.json at once, with no push from shinyctl-api, and knows which copies
// are draining: a visitor pinned to one keeps it, a new one never gets it.
func TestRun_ReadsCopiesFile(t *testing.T) {
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up") }))
	draining := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "draining") }))
	app := plainApp("a", freePort(t))
	home, _, s := proxyHome(t, app)
	copies, _ := json.Marshal(Table{"a": {Active: []string{up}, Draining: []string{draining}}})
	if err := os.WriteFile(s.CopiesPath, copies, 0600); err != nil {
		t.Fatal(err)
	}

	p := startSubproxyAt(t, s.ConfigPath, home)
	p.waitAnswering(t, Admin{Socket: s.AdminSocket})

	if status, body, _ := get(t, appURL(app)); status != 200 || body != "up" {
		t.Errorf("response = %d %q, want 200 up", status, body)
	}
	if got, err := (Admin{Socket: s.AdminSocket}).Copies(); err != nil || len(got["a"].Active) != 1 || len(got["a"].Draining) != 1 {
		t.Errorf("the table = %v, %v", got, err)
	}

	// The cookie of the draining copy keeps its visitor on it.
	mac := hmac.New(sha256.New, []byte(s.Secret))
	io.WriteString(mac, draining)
	req, _ := http.NewRequest("GET", appURL(app), nil)
	req.AddCookie(&http.Cookie{Name: CookieName(app.Port), Value: hex.EncodeToString(mac.Sum(nil))})
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "draining" {
		t.Errorf("a visitor pinned to the draining copy got %q, want it kept there", body)
	}
}

// TestRun_UnusableCopiesFileServesNoCopies: a copies.json that can't be read
// is not fatal; the proxy runs, says so, and waits for shinyctl-api.
func TestRun_UnusableCopiesFileServesNoCopies(t *testing.T) {
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up") }))
	app := plainApp("a", freePort(t))
	home, _, s := proxyHome(t, app)
	s.TryDuration = time.Second
	cfg, err := Render(s, []App{app})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ConfigPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.CopiesPath, []byte("{ not json"), 0600); err != nil {
		t.Fatal(err)
	}

	p := startSubproxyAt(t, s.ConfigPath, home)
	admin := Admin{Socket: s.AdminSocket}
	p.waitAnswering(t, admin)

	want := "could not read " + s.CopiesPath + ": the file is not usable; serving no copies until shinyctl-api sends them"
	if !strings.Contains(p.stderr.String(), want) {
		t.Errorf("stderr = %q, want it to say %q", p.stderr.String(), want)
	}
	if productName.MatchString(p.stderr.String()) {
		t.Errorf("stderr names the software:\n%s", p.stderr.String())
	}
	if status, body, _ := get(t, appURL(app)); status != 503 || !strings.Contains(body, "This app is restarting") {
		t.Errorf("response = %d %q, want the restarting page", status, body)
	}

	// shinyctl-api's push puts it right.
	if err := admin.PutCopies(Table{"a": {Active: []string{up}}}); err != nil {
		t.Fatal(err)
	}
	if status, body, _ := get(t, appURL(app)); status != 200 || body != "up" {
		t.Errorf("after the push: %d %q", status, body)
	}
}

// What the engine logs reaches api.log as plain lines, not as JSON.
func TestRun_EngineLogIsPlain(t *testing.T) {
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // the copy dies in the middle of the response
	}))
	app := servedApp(t, "a", freePort(t), up)
	home, _, s := proxyHome(t, app)
	p := startSubproxyAt(t, s.ConfigPath, home)
	p.waitAnswering(t, Admin{Socket: s.AdminSocket})

	resp, err := http.Get(appURL(app) + "some/page")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body) // fails half way, as it should
	resp.Body.Close()

	want := fmt.Sprintf("proxy: the copy at %s stopped in the middle of a response (GET /some/page)", up)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(p.stderr.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("stderr has no line starting %q:\n%s", want, p.stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, line := range strings.Split(p.stderr.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			t.Errorf("a line of the engine's JSON reached the log: %s", line)
		}
		if strings.Contains(strings.ToLower(line), "caddy") {
			t.Errorf("the engine is named in the log: %s", line)
		}
	}
}

// A load balancer started with --after serves next to the one that holds the
// lock, says so on the control socket, then stops the old one itself and takes
// the lock. The apps' ports answer throughout, and the control socket is the
// new one's afterwards and takes a reload.
func TestRun_TakesOverFromTheRunningOne(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_HANDOVER_GRACE", "1s")
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up") }))
	app := servedApp(t, "a", freePort(t), up)
	home, _, s := proxyHome(t, app)
	admin := Admin{Socket: s.AdminSocket}

	a := startSubproxyAt(t, s.ConfigPath, home)
	a.waitAnswering(t, admin)
	waitFor(t, "the first to serve", 5*time.Second, func() bool { pid, _ := admin.ServingPID(); return pid == a.pid() })

	stop := hammer(appURL(app))
	b := startSubproxyArgs(t, s.ConfigPath, home, "--after", fmt.Sprint(a.pid()))
	waitFor(t, "the second to say it serves", 10*time.Second, func() bool {
		select {
		case <-b.exited:
			t.Fatalf("the second exited: %v\n%s", b.err, b.stderr.String())
		default:
		}
		pid, _ := admin.ServingPID()
		return pid == b.pid()
	})
	if info, _ := readLock(LockPath(s.ConfigPath)); info.PID != a.pid() {
		t.Errorf("lock = %+v while both serve, want it to still name the first (%d)", info, a.pid())
	}
	if !alive(a.pid()) {
		t.Error("the first was stopped before the grace period passed")
	}

	// Nobody signals the first but the second.
	select {
	case <-a.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the second did not stop the first")
	}
	if a.err != nil {
		t.Errorf("the first was stopped with %v, want a clean exit on SIGTERM", a.err)
	}
	waitFor(t, "the second to take the lock", 5*time.Second, func() bool {
		info, _ := readLock(LockPath(s.ConfigPath))
		return info.PID == b.pid()
	})
	// The control socket is the second's: it names it, and a reload through it works.
	if pid, err := admin.ServingPID(); err != nil || pid != b.pid() {
		t.Errorf("the control socket names pid %d (%v) after the handover, want %d", pid, err, b.pid())
	}
	cfg, err := os.ReadFile(s.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Load(cfg); err != nil {
		t.Errorf("a reload through the control socket after the handover: %v", err)
	}
	// The requests the old one is answering finish (see
	// TestRun_StopAnswersTheRequestsInFlight). On Linux the kernel keeps
	// hashing new connections to the old socket until it closes, so a request
	// that reaches it in that instant can still fail once: in 15 handovers of
	// this build with 4 clients, one had two such failures, none a refusal.
	// macOS hands all new connections to the newest socket.
	allowed := 0
	if runtime.GOOS == "linux" {
		allowed = 2
	}
	if ok, failed := stop(); failed > allowed || ok == 0 {
		t.Errorf("%d requests answered, %d failed, want at most %d failed while the load balancer changed hands", ok, failed, allowed)
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// A takeover whose old one never lets go of the lock gives up after
// takeoverWait: it stops serving and exits with an error.
func TestRun_TakeoverGivesUpWhenTheOldOneNeverLetsGo(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_HANDOVER_GRACE", "10ms")
	t.Setenv("SHINYCTL_TEST_TAKEOVER_WAIT", "1s")
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up") }))
	app := servedApp(t, "a", freePort(t), up)
	home, _, s := proxyHome(t, app)

	// The old one is this process: it holds the lock, and is not a load
	// balancer, so nothing signals it.
	lock, err := acquireLock(LockPath(s.ConfigPath), lockInfo{PID: os.Getpid(), Build: "old"})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	b := startSubproxyArgs(t, s.ConfigPath, home, "--after", fmt.Sprint(os.Getpid()))
	select {
	case <-b.exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("the second did not give up\n%s", b.stderr.String())
	}
	if b.cmd.ProcessState.ExitCode() == 0 {
		t.Errorf("the second exited cleanly, want an error:\n%s", b.stderr.String())
	}
	if !strings.Contains(b.stderr.String(), "is still running") {
		t.Errorf("stderr = %q, want it to say the one it replaces is still running", b.stderr.String())
	}
	waitFor(t, "the port to close", 3*time.Second, func() bool { return refused(app.Addr()) })
	if info, _ := readLock(LockPath(s.ConfigPath)); info.PID != os.Getpid() {
		t.Errorf("lock = %+v, want it untouched", info)
	}
}

// A second takeover from the same load balancer, started while the first is
// still waiting, loses: the first one takes the lock, and the second stops
// serving at once instead of serving next to it.
func TestRun_ASecondTakeoverStepsAside(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_HANDOVER_GRACE", "300ms")
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up") }))
	app := servedApp(t, "a", freePort(t), up)
	home, _, s := proxyHome(t, app)
	admin := Admin{Socket: s.AdminSocket}
	a := startSubproxyAt(t, s.ConfigPath, home)
	a.waitAnswering(t, admin)

	b := startSubproxyArgs(t, s.ConfigPath, home, "--after", fmt.Sprint(a.pid()))
	waitFor(t, "the first replacement to serve", 10*time.Second, func() bool { pid, _ := admin.ServingPID(); return pid == b.pid() })
	c := startSubproxyArgs(t, s.ConfigPath, home, "--after", fmt.Sprint(a.pid()))
	select {
	case <-a.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the first was not stopped")
	}
	var loser, winner *subproxy
	select {
	case <-b.exited:
		loser, winner = b, c
	case <-c.exited:
		loser, winner = c, b
	case <-time.After(10 * time.Second):
		t.Fatal("neither replacement stepped aside")
	}
	if loser.cmd.ProcessState.ExitCode() == 0 || !strings.Contains(loser.stderr.String(), "took over first") {
		t.Errorf("the loser exited with %v and said %q, want an error that another took over first", loser.err, loser.stderr.String())
	}
	waitFor(t, "the winner to hold the lock", 5*time.Second, func() bool {
		info, _ := readLock(LockPath(s.ConfigPath))
		return info.PID == winner.pid()
	})
	if status, body, _ := get(t, appURL(app)); status != 200 || body != "up" {
		t.Errorf("the app answers %d %q, want 200 up", status, body)
	}
}

// With no shinyctl-api at all, a load balancer started with --after finishes
// the handover by itself.
func TestRun_FinishesTheHandoverWithoutShinyctlAPI(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_HANDOVER_GRACE", "100ms")
	home, _, s := proxyHome(t)
	admin := Admin{Socket: s.AdminSocket}
	a := startSubproxyAt(t, s.ConfigPath, home)
	a.waitAnswering(t, admin)
	b := startSubproxyArgs(t, s.ConfigPath, home, "--after", fmt.Sprint(a.pid()))
	select {
	case <-a.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the first was not stopped")
	}
	waitFor(t, "the second to take the lock", 5*time.Second, func() bool {
		info, _ := readLock(LockPath(s.ConfigPath))
		return info.PID == b.pid()
	})
	if pid, err := admin.ServingPID(); err != nil || pid != b.pid() {
		t.Errorf("the control socket names pid %d (%v), want %d", pid, err, b.pid())
	}
}

// The old one is signalled only after the new one serves: with the new one
// held back, the old one keeps the lock and keeps running.
func TestRun_TheOldOneIsNotStoppedBeforeTheNewOneServes(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_HANDOVER_GRACE", "10ms")
	t.Setenv("SHINYCTL_TEST_SERVE_DELAY", "1500ms")
	home, _, s := proxyHome(t)
	admin := Admin{Socket: s.AdminSocket}
	a := startSubproxyAt(t, s.ConfigPath, home)
	a.waitAnswering(t, admin)
	b := startSubproxyArgs(t, s.ConfigPath, home, "--after", fmt.Sprint(a.pid()))

	servedAt := time.Time{}
	deadline := time.Now().Add(10 * time.Second)
	for servedAt.IsZero() && time.Now().Before(deadline) {
		if pid, _ := admin.ServingPID(); pid == b.pid() {
			servedAt = time.Now()
			break
		}
		select {
		case <-a.exited:
			t.Fatal("the old one went before the new one said it serves")
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if servedAt.IsZero() {
		t.Fatal("the new one never said it serves")
	}
	select {
	case <-a.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the old one was not stopped once the new one served")
	}
}

// A takeover that can't load its settings exits and leaves the one that
// serves alone; it never serves nothing next to it.
func TestRun_TakeoverThatCannotLoadExits(t *testing.T) {
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up") }))
	app := servedApp(t, "a", freePort(t), up)
	home, _, s := proxyHome(t, app)
	admin := Admin{Socket: s.AdminSocket}

	a := startSubproxyAt(t, s.ConfigPath, home)
	a.waitAnswering(t, admin)

	if err := os.WriteFile(s.ConfigPath, []byte("{ this is not json"), 0600); err != nil {
		t.Fatal(err)
	}
	b := startSubproxyArgs(t, s.ConfigPath, home, "--after", fmt.Sprint(a.pid()))
	select {
	case <-b.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the second did not exit")
	}
	if b.cmd.ProcessState.ExitCode() == 0 {
		t.Errorf("the second exited cleanly, want an error:\n%s", b.stderr.String())
	}
	if !strings.Contains(b.stderr.String(), "next to the running load balancer") {
		t.Errorf("stderr = %q, want it to say it could not load next to the running one", b.stderr.String())
	}
	if pid, _ := admin.ServingPID(); pid != a.pid() {
		t.Errorf("the control socket is answered by pid %d, want the first (%d)", pid, a.pid())
	}
	if info, _ := readLock(LockPath(s.ConfigPath)); info.PID != a.pid() {
		t.Errorf("lock = %+v, want the first (%d)", info, a.pid())
	}
	if status, body, _ := get(t, appURL(app)); status != 200 || body != "up" {
		t.Errorf("the app answers %d %q, want 200 up", status, body)
	}
}

// --after names a process that doesn't hold the lock: nothing to take over
// from, so it is an ordinary start, and the lock refuses it.
func TestRun_AfterAnotherPidIsAnOrdinaryStart(t *testing.T) {
	home, _, s := proxyHome(t)
	admin := Admin{Socket: s.AdminSocket}
	a := startSubproxyAt(t, s.ConfigPath, home)
	a.waitAnswering(t, admin)

	b := startSubproxyArgs(t, s.ConfigPath, home, "--after", fmt.Sprint(os.Getpid()))
	select {
	case <-b.exited:
	case <-time.After(3 * time.Second):
		t.Fatal("the second did not exit")
	}
	if want := fmt.Sprintf("already running (pid %d)", a.pid()); !strings.Contains(b.stderr.String(), want) {
		t.Errorf("stderr = %q, want %q", b.stderr.String(), want)
	}
	if err := admin.Ping(); err != nil {
		t.Errorf("the first stopped answering: %v", err)
	}
}

// Checking settings, as shinyctl-api does before it sends them, leaves no line
// of the engine's JSON in api.log either.
func TestValidate_EngineLogIsPlain(t *testing.T) {
	dir := shortDir(t)
	cfg, err := Render(settingsFor(dir), []App{plainApp("a", freePort(t))})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "check.json")
	if err := os.WriteFile(path, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "validate", path)
	cmd.Env = append(os.Environ(), "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			t.Errorf("a line of the engine's JSON reached the log: %s", line)
		}
	}
}

// A load balancer that gets SIGTERM answers the requests it is in the middle
// of before it exits: with the upstream taking 30 ms or 300 ms every one of
// six requests in flight answers 200.
func TestRun_StopAnswersTheRequestsInFlight(t *testing.T) {
	for _, d := range []time.Duration{30 * time.Millisecond, 300 * time.Millisecond} {
		var arrived atomic.Int32
		up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			arrived.Add(1)
			time.Sleep(d)
			io.WriteString(w, "up")
		}))
		app := servedApp(t, "a", freePort(t), up)
		home, _, s := proxyHome(t, app)
		admin := Admin{Socket: s.AdminSocket}
		p := startSubproxyAt(t, s.ConfigPath, home)
		p.waitAnswering(t, admin)
		if code, _, _ := get(t, appURL(app)); code != 200 {
			t.Fatalf("not serving: %d", code)
		}
		arrived.Store(0)
		results := inFlightGets(app, 6)
		// All six have reached the upstream, so none is still on its way to
		// the load balancer (or in the kernel's queue) when it is signalled.
		waitFor(t, "all six requests to reach the upstream", 10*time.Second, func() bool { return arrived.Load() == 6 })
		signalled := time.Now()
		p.signal(syscall.SIGTERM)
		<-p.exited
		// It goes soon after the last request: the wait polls often.
		if took := time.Since(signalled); took > d+500*time.Millisecond {
			t.Errorf("upstream %v: the load balancer took %s to go, want it soon after the last request", d, took)
		}
		if p.err != nil {
			t.Errorf("upstream %v: exit = %v, want a clean exit", d, p.err)
		}
		for i := 0; i < 6; i++ {
			if got := <-results; got != "200" {
				t.Errorf("upstream %v: a request in flight at SIGTERM got %s, want 200", d, got)
			}
		}
	}
}

// inFlightGets starts n requests on connections of their own and returns a
// channel of their outcomes: the status, or "error".
func inFlightGets(app App, n int) <-chan string {
	results := make(chan string, n)
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < n; i++ {
		go func() {
			resp, err := client.Get(appURL(app))
			if err != nil {
				results <- "error"
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			results <- fmt.Sprint(resp.StatusCode)
		}()
	}
	return results
}

// A request that takes longer than stopTimeout is cut at about stopTimeout,
// and the process does not wait for a WebSocket at all.
func TestRun_StopWaitsNoLongerThanStopTimeout(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_STOP_TIMEOUT", "1s")
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			rw.Flush()
			io.Copy(io.Discard, conn) // a session: it never ends by itself
			return
		}
		time.Sleep(4 * time.Second)
		io.WriteString(w, "late")
	}))
	app := servedApp(t, "a", freePort(t), up)
	home, _, s := proxyHome(t, app)
	admin := Admin{Socket: s.AdminSocket}
	p := startSubproxyAt(t, s.ConfigPath, home)
	p.waitAnswering(t, admin)

	// A session first: it must not be waited for.
	conn, err := net.Dial("tcp", app.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	line := make([]byte, 12)
	if _, err := io.ReadFull(conn, line); err != nil || string(line) != "HTTP/1.1 101" {
		t.Fatalf("the session did not start: %q, %v", line, err)
	}

	results := inFlightGets(app, 3)
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	p.signal(syscall.SIGTERM)
	<-p.exited
	took := time.Since(start)
	if took < 800*time.Millisecond || took > 2500*time.Millisecond {
		t.Errorf("the load balancer took %s to go, want about the stop timeout (1s)", took)
	}
	for i := 0; i < 3; i++ {
		if got := <-results; got != "error" {
			t.Errorf("a request that outlasts the stop timeout got %s, want it cut", got)
		}
	}
}

// The new load balancer signals the old one only while the lock record still
// names it. Here the record names someone else by the time the grace period is
// over, though the old one's command line is still a load balancer's for this
// config: it is left alone.
func TestRun_NeverSignalsAProcessTheLockDoesNotName(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_HANDOVER_GRACE", "1500ms")
	home, _, s := proxyHome(t)
	admin := Admin{Socket: s.AdminSocket}
	a := startSubproxyAt(t, s.ConfigPath, home)
	a.waitAnswering(t, admin)
	b := startSubproxyArgs(t, s.ConfigPath, home, "--after", fmt.Sprint(a.pid()))
	waitFor(t, "the new one to say it serves", 10*time.Second, func() bool {
		pid, _ := admin.ServingPID()
		return pid == b.pid()
	})
	// The record changes under the first one's flock.
	rec, _ := json.Marshal(lockInfo{PID: os.Getpid(), Build: "someone else"})
	if err := os.WriteFile(LockPath(s.ConfigPath), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the new one did not give up")
	}
	if !alive(a.pid()) {
		t.Error("the old one was signalled although the lock record no longer names it")
	}
}

// A WebSocket is a request that never ends by itself: stopping does not wait
// for it, however long the stop timeout is.
func TestRun_StopDoesNotWaitForSessions(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_STOP_TIMEOUT", "4s")
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		io.Copy(io.Discard, conn)
	}))
	app := servedApp(t, "a", freePort(t), up)
	home, _, s := proxyHome(t, app)
	p := startSubproxyAt(t, s.ConfigPath, home)
	p.waitAnswering(t, Admin{Socket: s.AdminSocket})

	conn, err := net.Dial("tcp", app.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	line := make([]byte, 12)
	if _, err := io.ReadFull(conn, line); err != nil || string(line) != "HTTP/1.1 101" {
		t.Fatalf("the session did not start: %q, %v", line, err)
	}
	start := time.Now()
	p.signal(syscall.SIGTERM)
	<-p.exited
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the load balancer took %s to stop with a session open, want it not to wait for the session", took)
	}
}

// A request that offers an upgrade to h2c (curl --http2 does) is answered as
// plain HTTP/1.1, so it is one of the requests in flight that a stop waits for.
func TestRun_StopAnswersARequestThatOffersH2c(t *testing.T) {
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		io.WriteString(w, "up")
	}))
	app := servedApp(t, "a", freePort(t), up)
	home, _, s := proxyHome(t, app)
	p := startSubproxyAt(t, s.ConfigPath, home)
	p.waitAnswering(t, Admin{Socket: s.AdminSocket})

	const n = 4
	results := make(chan string, n)
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < n; i++ {
		go func() {
			req, _ := http.NewRequest(http.MethodGet, appURL(app), nil)
			req.Header.Set("Connection", "Upgrade, HTTP2-Settings")
			req.Header.Set("Upgrade", "h2c")
			req.Header.Set("HTTP2-Settings", "AAMAAABkAAQAAP__")
			resp, err := client.Do(req)
			if err != nil {
				results <- "error"
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			results <- fmt.Sprint(resp.StatusCode)
		}()
	}
	time.Sleep(50 * time.Millisecond)
	p.signal(syscall.SIGTERM)
	<-p.exited
	for i := 0; i < n; i++ {
		if got := <-results; got != "200" {
			t.Errorf("a request offering h2c, in flight at SIGTERM, got %s, want 200", got)
		}
	}
}

// The wait for requests in flight is counted from the signal, together with
// the engine's stop: a slow stop does not give a long request more time.
func TestRun_TheStopTimeoutIsCountedFromTheSignal(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_STOP_TIMEOUT", "1s")
	t.Setenv("SHINYCTL_TEST_STOP_DELAY", "600ms")
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		io.WriteString(w, "late")
	}))
	app := servedApp(t, "a", freePort(t), up)
	home, _, s := proxyHome(t, app)
	p := startSubproxyAt(t, s.ConfigPath, home)
	p.waitAnswering(t, Admin{Socket: s.AdminSocket})
	inFlightGets(app, 2)
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	p.signal(syscall.SIGTERM)
	<-p.exited
	if took := time.Since(start); took < 800*time.Millisecond || took > 1300*time.Millisecond {
		t.Errorf("the load balancer took %s to go, want about the 1s stop timeout, counted from the signal", took)
	}
}
