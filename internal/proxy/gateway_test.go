package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// gwEnv is a Gateway over a real proxy process.
type gwEnv struct {
	*supEnv
	sup *Supervisor
	gw  *Gateway
}

func newGwEnv(t *testing.T) *gwEnv {
	t.Helper()
	e := &gwEnv{supEnv: newSupEnv(t)}
	e.cleanupProxy(t)
	e.sup = e.supervisor(t, "")
	run(t, e.sup)
	if !e.sup.WaitReady(10 * time.Second) {
		t.Fatal("the load balancer did not become ready")
	}
	e.gw = NewGateway(e.settings, e.sup)
	return e
}

// app is an app on a public port whose active copy listens at dial, in the
// table of this env's proxy.
func (e *gwEnv) app(t *testing.T, key string, publicPort int, dial string) App {
	t.Helper()
	if err := e.gw.SetPool(key, Pool{Active: []string{dial}}); err != nil {
		t.Fatal(err)
	}
	return plainApp(key, publicPort)
}

func okUpstream(t *testing.T, body string) string {
	return startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
}

func served(t *testing.T, a App, body string) {
	t.Helper()
	if status, got, _ := get(t, appURL(a)); status != 200 || got != body {
		t.Errorf("%s answers %d %q, want 200 %q", a.Addr(), status, got, body)
	}
}

func configApps(t *testing.T, path string) []App {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	apps, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return apps
}

func TestGateway_SyncServesAndRemoves(t *testing.T) {
	e := newGwEnv(t)
	a := e.app(t, "a", freePort(t), okUpstream(t, "a"))

	left, err := e.gw.Sync([]App{a})
	if err != nil || len(left) != 0 {
		t.Fatalf("Sync = %v, %v", left, err)
	}
	served(t, a, "a")
	if !e.gw.Serves("a", "127.0.0.1", a.Port) {
		t.Error("Serves(a) = false after Sync")
	}

	if _, err := e.gw.Sync(nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a's port to close", 2*time.Second, func() bool { return refused(a.Addr()) })
	if e.gw.Serves("a", "127.0.0.1", a.Port) {
		t.Error("Serves(a) = true after it was removed")
	}

	want, _ := Render(e.settings, nil)
	got, err := os.ReadFile(e.settings.ConfigPath)
	if err != nil || string(got) != string(want) {
		t.Errorf("config.json = %q, %v, want the last loaded config (no apps)", got, err)
	}
}

func TestGateway_LeavesOutTakenPort(t *testing.T) {
	e := newGwEnv(t)
	a := e.app(t, "a", freePort(t), okUpstream(t, "a"))
	taken := freePort(t)
	ln := hold(t, taken)
	b := e.app(t, "b", taken, okUpstream(t, "b"))

	left, err := e.gw.Sync([]App{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left["b"] == nil {
		t.Fatalf("left = %v, want only b", left)
	}
	bind, ok := left["b"].(*BindError)
	if !ok || bind.Addr != b.Addr() {
		t.Fatalf("left[b] = %v (%T), want a *BindError for %s", left["b"], left["b"], b.Addr())
	}
	served(t, a, "a")
	if why := e.gw.NotServed("b"); !strings.Contains(why, "is not available") {
		t.Errorf("NotServed(b) = %q, want the bind error", why)
	}

	if apps := configApps(t, e.settings.ConfigPath); len(apps) != 1 || apps[0].Key != "a" {
		t.Errorf("config.json serves %v, want only a", apps)
	}
	if e.gw.loads != 1 || e.gw.attempts != 2 {
		t.Errorf("loads = %d, attempts = %d, want 1 and 2 (one rejected, one accepted)", e.gw.loads, e.gw.attempts)
	}

	// The same failure is not tried against the load balancer again: every
	// rejected reload is an error line in the log.
	left, err = e.gw.Sync([]App{a, b})
	if err != nil || left["b"] == nil {
		t.Fatalf("second Sync = %v, %v, want b still left out", left, err)
	}
	if e.gw.loads != 1 || e.gw.attempts != 2 {
		t.Errorf("after the second Sync, loads = %d, attempts = %d, want 1 and 2", e.gw.loads, e.gw.attempts)
	}

	// Once the port is free the next Sync serves b.
	ln.Close()
	left, err = e.gw.Sync([]App{a, b})
	if err != nil || len(left) != 0 {
		t.Fatalf("third Sync = %v, %v, want both served", left, err)
	}
	served(t, b, "b")
	if why := e.gw.NotServed("b"); why != "" {
		t.Errorf("NotServed(b) = %q once it is served", why)
	}
}

// TestGateway_NonCanonicalHostOnTakenPort: a host written in a longer form
// is named by the OS in its canonical form. The app is left out like any
// other whose port is taken, and does not make every sync fail.
func TestGateway_NonCanonicalHostOnTakenPort(t *testing.T) {
	cases := []struct {
		host   string
		holder func(t *testing.T, port int) net.Listener
	}{
		{"::ffff:127.0.0.1", func(t *testing.T, port int) net.Listener { return hold(t, port) }},
		{"0:0:0:0:0:0:0:1", func(t *testing.T, port int) net.Listener {
			ln, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
			if err != nil {
				t.Skipf("no IPv6 loopback: %v", err)
			}
			t.Cleanup(func() { ln.Close() })
			return ln
		}},
	}

	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			e := newGwEnv(t)
			taken := freePort(t)
			c.holder(t, taken)
			a := e.app(t, "a", taken, okUpstream(t, "a"))
			a.Host = c.host
			b := e.app(t, "b", freePort(t), okUpstream(t, "b"))

			left, err := e.gw.Sync([]App{a, b})
			if err != nil {
				t.Fatalf("Sync error = %v, want only a to be left out", err)
			}
			if _, ok := left["a"].(*BindError); !ok || len(left) != 1 {
				t.Fatalf("left = %v, want a *BindError for a only", left)
			}
			served(t, b, "b")
			if e.gw.loads != 1 || e.gw.attempts != 2 {
				t.Errorf("loads = %d, attempts = %d, want 1 and 2", e.gw.loads, e.gw.attempts)
			}

			// Not tried again, and b's changes still go through.
			c2 := e.app(t, "c", freePort(t), okUpstream(t, "c"))
			left, err = e.gw.Sync([]App{a, b, c2})
			if err != nil || left["a"] == nil {
				t.Fatalf("second Sync = %v, %v", left, err)
			}
			served(t, c2, "c")
			if e.gw.attempts != 3 {
				t.Errorf("attempts = %d, want 3 (the new app's reload only)", e.gw.attempts)
			}
		})
	}
}

func TestGateway_ConflictingApps(t *testing.T) {
	e := newGwEnv(t)
	port := freePort(t)
	a := e.app(t, "a", port, okUpstream(t, "a"))
	b := e.app(t, "b", port, okUpstream(t, "b"))

	left, err := e.gw.Sync([]App{a, b})
	if err != nil {
		t.Fatal(err)
	}
	want := PortUsedBy(b, a).Error()
	if left["b"] == nil || left["b"].Error() != want {
		t.Errorf("left[b] = %v, want %q", left["b"], want)
	}
	served(t, a, "a")
}

func TestGateway_NoReloadWhenUnchanged(t *testing.T) {
	e := newGwEnv(t)
	a := e.app(t, "a", freePort(t), okUpstream(t, "a"))

	for i := 0; i < 2; i++ {
		if _, err := e.gw.Sync([]App{a}); err != nil {
			t.Fatal(err)
		}
	}
	if e.gw.loads != 1 {
		t.Errorf("loads = %d, want 1", e.gw.loads)
	}
}

func TestGateway_NoReloadAfterAdoption(t *testing.T) {
	e := newSupEnv(t)
	e.cleanupProxy(t)
	a := plainApp("a", freePort(t))
	up := okUpstream(t, "a")

	supA := e.supervisor(t, "")
	stopA := run(t, supA)
	if !supA.WaitReady(10 * time.Second) {
		t.Fatal("the load balancer did not become ready")
	}
	gwA := NewGateway(e.settings, supA)
	if err := gwA.SetPool(a.Key, Pool{Active: []string{up}}); err != nil {
		t.Fatal(err)
	}
	if _, err := gwA.Sync([]App{a}); err != nil {
		t.Fatal(err)
	}
	stopA()

	before, err := os.Stat(e.settings.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	supB := e.supervisor(t, "")
	run(t, supB)
	if !supB.WaitReady(10 * time.Second) {
		t.Fatal("the second supervisor did not become ready")
	}
	if supB.PID() != supA.PID() {
		t.Fatalf("pid = %d, want the adopted %d", supB.PID(), supA.PID())
	}
	gwB := NewGateway(e.settings, supB)
	if left, err := gwB.Sync([]App{a}); err != nil || len(left) != 0 {
		t.Fatalf("Sync = %v, %v", left, err)
	}
	if gwB.loads != 0 {
		t.Errorf("loads = %d, want 0: adopting a proxy must not reload it", gwB.loads)
	}
	after, err := os.Stat(e.settings.ConfigPath)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("config.json was rewritten (%v -> %v, %v)", before.ModTime(), after.ModTime(), err)
	}
	served(t, a, "a")
}

// fakeProc is a proc that is ready or not as the test says.
type fakeProc struct{ ready bool }

func (f fakeProc) WaitReady(time.Duration) bool { return f.ready }
func (f fakeProc) Running() bool                { return f.ready }
func (f fakeProc) Generation() int              { return 1 }
func (f fakeProc) PID() int                     { return 4242 }

func TestGateway_RejectedConfigNotWritten(t *testing.T) {
	s := settingsFor(shortDir(t))
	s.AdminSocket = filepath.Join(filepath.Dir(s.ConfigPath), "fake.sock")
	ln, err := net.Listen("unix", s.AdminSocket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/load" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"boom"}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	const marker = "last good config"
	if err := os.WriteFile(s.ConfigPath, []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}

	g := newGateway(s, fakeProc{ready: true})
	a := plainApp("a", freePort(t))
	if _, err := g.Sync([]App{a}); err != errRejected {
		t.Errorf("Sync error = %v, want errRejected", err)
	}
	if got, _ := os.ReadFile(s.ConfigPath); string(got) != marker {
		t.Errorf("config.json = %q after a rejected load, want it untouched", got)
	}
}

func TestGateway_NotRunning(t *testing.T) {
	g := newGateway(settingsFor(shortDir(t)), fakeProc{ready: false})
	g.readyWait = 50 * time.Millisecond
	if _, err := g.Sync(nil); err != ErrNotRunning {
		t.Errorf("Sync error = %v, want ErrNotRunning", err)
	}
	if g.Serves("a", "127.0.0.1", 1) || g.Served() != nil {
		t.Error("a load balancer that is not running serves nothing")
	}
}

// TestGateway_InvalidSettingsAreNotSent: settings that fail the check never
// reach the load balancer.
func TestGateway_InvalidSettingsAreNotSent(t *testing.T) {
	s := settingsFor(shortDir(t))
	s.AdminSocket = filepath.Join(filepath.Dir(s.ConfigPath), "fake.sock")
	ln, err := net.Listen("unix", s.AdminSocket)
	if err != nil {
		t.Fatal(err)
	}
	var loads atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/load" {
			loads.Add(1)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	g := newGateway(s, fakeProc{ready: true})
	bad := plainApp("a", 70000) // not a port
	if _, err := g.Sync([]App{bad}); err != errInvalid {
		t.Errorf("Sync error = %v, want errInvalid", err)
	}
	if loads.Load() != 0 || g.attempts != 0 {
		t.Errorf("the load balancer was sent %d load(s), attempts = %d, want none", loads.Load(), g.attempts)
	}
}

// TestGateway_NotRunningServesNothing: what the load balancer last served is
// not reported while it is down, even when its socket still answers.
func TestGateway_NotRunningServesNothing(t *testing.T) {
	s := settingsFor(shortDir(t))
	s.AdminSocket = filepath.Join(filepath.Dir(s.ConfigPath), "fake.sock")
	ln, err := net.Listen("unix", s.AdminSocket)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Render(s, []App{plainApp("a", 8080)})
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(cfg) })}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	g := newGateway(s, fakeProc{ready: false})
	if got := g.Served(); got != nil {
		t.Errorf("Served() = %v, want nothing while the load balancer is not running", got)
	}
	if g.Serves("a", "127.0.0.1", 8080) {
		t.Error("Serves reports an app while the load balancer is not running")
	}
	up := newGateway(s, fakeProc{ready: true})
	if got := up.Served(); len(got) != 1 {
		t.Errorf("Served() = %v when running, want the one app", got)
	}
}

// TestGateway_RestartedBalancerServesLastGoodConfig: a load balancer that
// dies and is started again comes back with the apps it had, before any sync.
func TestGateway_RestartedBalancerServesLastGoodConfig(t *testing.T) {
	e := newGwEnv(t)
	a := e.app(t, "a", freePort(t), okUpstream(t, "a"))
	if _, err := e.gw.Sync([]App{a}); err != nil {
		t.Fatal(err)
	}
	served(t, a, "a")

	old := e.sup.PID()
	syscall.Kill(old, syscall.SIGKILL)
	waitFor(t, "a new load balancer", 5*time.Second, func() bool { return e.sup.Running() && e.sup.PID() != old })
	waitFor(t, "the app to be served again", 5*time.Second, func() bool {
		resp, err := http.Get(appURL(a))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	})
}

// TestBindsTo: a bind error names an address the way the OS does; the app it
// is about is found whatever way the host was written, and not by accident.
func TestBindsTo(t *testing.T) {
	cases := []struct {
		name string
		app  App
		addr string
		want bool
	}{
		{"same address", App{Host: "127.0.0.1", Port: 9}, "127.0.0.1:9", true},
		{"mapped form", App{Host: "::ffff:127.0.0.1", Port: 9}, "127.0.0.1:9", true},
		{"long IPv6 form", App{Host: "0:0:0:0:0:0:0:1", Port: 9}, "[::1]:9", true},
		{"error in a form the app isn't", App{Host: "127.0.0.1", Port: 9}, "[::ffff:127.0.0.1]:9", true},
		{"the wildcard overlaps", App{Host: "0.0.0.0", Port: 9}, "127.0.0.1:9", true},
		{"another port", App{Host: "127.0.0.1", Port: 9}, "127.0.0.1:10", false},
		{"another family", App{Host: "127.0.0.1", Port: 9}, "[::1]:9", false},
		{"not an address", App{Host: "127.0.0.1", Port: 9}, "nonsense", false},
	}
	for _, c := range cases {
		if got := bindsTo(c.app, c.addr); got != c.want {
			t.Errorf("%s: bindsTo(%+v, %q) = %v, want %v", c.name, c.app, c.addr, got, c.want)
		}
	}
}

// TestGateway_RetriedWideningIsProbedBesideItsOwnSocket: an address that
// failed before and overlaps the one the app is served at is probed the way
// the load balancer binds, or the app's own socket would count against it
// and a correct retry would be refused.
func TestGateway_RetriedWideningIsProbedBesideItsOwnSocket(t *testing.T) {
	e := newGwEnv(t)
	a := e.app(t, "a", freePort(t), okUpstream(t, "a"))
	if _, err := e.gw.Sync([]App{a}); err != nil {
		t.Fatal(err)
	}

	wide := a
	wide.Host = "0.0.0.0"
	e.gw.failed[wide.Addr()] = true
	var plain, beside int
	e.gw.available = func(string, int) error { plain++; return fmt.Errorf("own socket in the way") }
	e.gw.availableAlongside = func(string, int) error { beside++; return nil }

	left, err := e.gw.Sync([]App{wide})
	if err != nil || len(left) != 0 {
		t.Fatalf("Sync = %v, %v, want the wide app served", left, err)
	}
	if plain != 0 || beside != 1 {
		t.Errorf("plain probes = %d, probes beside the own socket = %d, want 0 and 1", plain, beside)
	}
	if !e.gw.Serves("a", "0.0.0.0", wide.Port) {
		t.Error("the wide address is not served")
	}
}

// TestGateway_ForgetsAddressesNobodyAsksFor: an address that failed is not
// held against the app once it asks for a different one again.
func TestGateway_ForgetsAddressesNobodyAsksFor(t *testing.T) {
	e := newGwEnv(t)
	a := e.app(t, "a", freePort(t), okUpstream(t, "a"))
	e.gw.failed["0.0.0.0:"+fmt.Sprint(a.Port)] = true
	e.gw.failed["127.0.0.1:1"] = true

	if _, err := e.gw.Sync([]App{a}); err != nil {
		t.Fatal(err)
	}
	if len(e.gw.failed) != 0 {
		t.Errorf("failed = %v, want none left for addresses no app asks for", e.gw.failed)
	}
}

func samePool(a, b Pool) bool { return (Table{"k": a}).Equal(Table{"k": b}) }

func twoCopies(a, b int) Pool {
	return Pool{Active: []string{fmt.Sprintf("127.0.0.1:%d", a)}, Warm: []string{fmt.Sprintf("127.0.0.1:%d", b)}}
}

func TestGateway_SetPoolPushesOnlyChanges(t *testing.T) {
	e := newGwEnv(t)
	p := twoCopies(20000, 20001)

	for i := 0; i < 2; i++ {
		if err := e.gw.SetPool("x", p); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.gw.Pushes(); got != 1 {
		t.Errorf("after two equal SetPools, Pushes = %d, want 1", got)
	}

	q := twoCopies(20001, 20000)
	if err := e.gw.SetPool("x", q); err != nil {
		t.Fatal(err)
	}
	if got := e.gw.Pushes(); got != 2 {
		t.Errorf("after a different pool, Pushes = %d, want 2", got)
	}

	// A change of the warm list alone is a change.
	r := Pool{Active: q.Active, Warm: []string{"127.0.0.1:20009"}}
	if err := e.gw.SetPool("x", r); err != nil {
		t.Fatal(err)
	}
	if got := e.gw.Pushes(); got != 3 {
		t.Errorf("after a change of the warm list only, Pushes = %d, want 3", got)
	}
	// So is a change of the draining list alone.
	d := Pool{Active: q.Active, Warm: r.Warm, Draining: []string{"127.0.0.1:20008"}}
	if err := e.gw.SetPool("x", d); err != nil {
		t.Fatal(err)
	}
	if got := e.gw.Pushes(); got != 4 {
		t.Errorf("after a change of the draining list only, Pushes = %d, want 4", got)
	}
	if err := e.gw.SetPool("x", q); err != nil {
		t.Fatal(err)
	}

	got, err := Admin{Socket: e.settings.AdminSocket}.Copies()
	if err != nil || !got.Equal(Table{"x": q}) {
		t.Errorf("the load balancer has %v, %v; want %v", got, err, Table{"x": q})
	}
	data, err := os.ReadFile(e.settings.CopiesPath)
	var onDisk Table
	if err != nil || json.Unmarshal(data, &onDisk) != nil || !onDisk.Equal(Table{"x": q}) {
		t.Errorf("copies.json = %s, %v; want the last push", data, err)
	}
	if info, err := os.Stat(e.settings.CopiesPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("copies.json mode = %v, %v; want 0600", info, err)
	}
	if e.gw.Reloads() != 0 {
		t.Errorf("Reloads = %d, want 0: a pool is not a reload", e.gw.Reloads())
	}
}

// TestGateway_PushAfterRestart: a load balancer that was started again has
// forgotten the table, so the same pool is sent once more.
func TestGateway_PushAfterRestart(t *testing.T) {
	e := newGwEnv(t)
	p := twoCopies(20000, 20001)
	if err := e.gw.SetPool("x", p); err != nil {
		t.Fatal(err)
	}
	before := e.gw.Pushes()

	// What the new process reads at start would hide the push; take it away.
	os.Remove(e.settings.CopiesPath)
	old := e.sup.PID()
	syscall.Kill(old, syscall.SIGKILL)
	waitFor(t, "a new load balancer", 10*time.Second, func() bool { return e.sup.Running() && e.sup.PID() != old })
	waitFor(t, "its control socket", 10*time.Second, func() bool { return Admin{Socket: e.settings.AdminSocket}.Ping() == nil })

	if got, _ := (Admin{Socket: e.settings.AdminSocket}).Copies(); len(got) != 0 {
		t.Fatalf("the new load balancer already has %v", got)
	}
	if err := e.gw.SetPool("x", p); err != nil {
		t.Fatal(err)
	}
	if got := e.gw.Pushes(); got != before+1 {
		t.Errorf("Pushes = %d, want %d: an unchanged pool must be sent to a new process", got, before+1)
	}
	if got, err := (Admin{Socket: e.settings.AdminSocket}).Copies(); err != nil || !got.Equal(Table{"x": p}) {
		t.Errorf("the load balancer has %v, %v", got, err)
	}
}

func TestGateway_ReplacePoolsKeepsNewer(t *testing.T) {
	g := newGateway(settingsFor(shortDir(t)), fakeProc{ready: false})
	p1, p2, p3 := twoCopies(20000, 20001), twoCopies(20002, 20003), twoCopies(20004, 20005)

	g.SetPool("x", p1)
	since := g.PoolSeq()
	g.SetPool("y", p2) // set after the table below was built

	g.ReplacePools(Table{"x": p3}, since)
	if got, _ := g.Pool("x"); !samePool(got, p3) {
		t.Errorf("x = %v, want the rebuilt %v (nothing newer)", got, p3)
	}
	if got, ok := g.Pool("y"); !ok || !samePool(got, p2) {
		t.Errorf("y = %v, %v; want it kept: it is newer than the rebuilt table", got, ok)
	}

	g.SetPool("x", p1)
	g.ReplacePools(Table{"x": p3}, since)
	if got, _ := g.Pool("x"); !samePool(got, p1) {
		t.Errorf("x = %v, want the newer %v kept", got, p1)
	}

	g.ReplacePools(nil, g.PoolSeq())
	if _, ok := g.Pool("x"); ok {
		t.Error("x survived a rebuilt table that doesn't have it")
	}
}

func TestGateway_SyncPrunesPools(t *testing.T) {
	e := newGwEnv(t)
	x := e.app(t, "x", freePort(t), okUpstream(t, "x"))
	if err := e.gw.SetPool("x", twoCopies(20000, 20001)); err != nil {
		t.Fatal(err)
	}
	if err := e.gw.SetPool("y", twoCopies(20002, 20003)); err != nil {
		t.Fatal(err)
	}

	if _, err := e.gw.Sync([]App{x}); err != nil {
		t.Fatal(err)
	}
	got, err := Admin{Socket: e.settings.AdminSocket}.Copies()
	if err != nil || len(got) != 1 || len(got["x"].Active) != 1 {
		t.Errorf("the load balancer has %v, %v; want only x", got, err)
	}
	if _, ok := e.gw.Pool("y"); ok {
		t.Error("y's pool survived a Sync that does not serve y")
	}

	// Also when nothing needs reloading.
	if err := e.gw.SetPool("z", twoCopies(20004, 20005)); err != nil {
		t.Fatal(err)
	}
	reloads := e.gw.Reloads()
	if _, err := e.gw.Sync([]App{x}); err != nil {
		t.Fatal(err)
	}
	if got, _ := (Admin{Socket: e.settings.AdminSocket}).Copies(); len(got) != 1 || e.gw.Reloads() != reloads {
		t.Errorf("table %v, reloads %d -> %d; want z pruned without a reload", got, reloads, e.gw.Reloads())
	}
}

func TestGateway_SetPoolWhenNotRunning(t *testing.T) {
	g := newGateway(settingsFor(shortDir(t)), fakeProc{ready: false})
	g.readyWait = 5 * time.Second // a push must not use it

	start := time.Now()
	err := g.SetPool("x", twoCopies(20000, 20001))
	if err != ErrNotRunning {
		t.Errorf("SetPool = %v, want ErrNotRunning", err)
	}
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Errorf("SetPool took %v, want it to return at once", took)
	}
	if _, ok := g.Pool("x"); !ok {
		t.Error("the pool was not kept for the next push")
	}
	if g.Pushes() != 0 {
		t.Errorf("Pushes = %d, want 0", g.Pushes())
	}
}

func TestGateway_SessionsNeedARunningBalancer(t *testing.T) {
	g := newGateway(settingsFor(shortDir(t)), fakeProc{ready: false})
	if got := g.Sessions(); got != nil {
		t.Errorf("Sessions = %v, want nil", got)
	}
	if g.BalancerPID() != 4242 {
		t.Errorf("BalancerPID = %d", g.BalancerPID())
	}
}

func TestGateway_SessionsCountOpenRequests(t *testing.T) {
	e := newGwEnv(t)
	hold := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	defer release()
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-hold
		}
	}))
	app := e.app(t, "a", freePort(t), up)
	if _, err := e.gw.Sync([]App{app}); err != nil {
		t.Fatal(err)
	}
	served(t, app, "")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.Get(appURL(app) + "hold"); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	waitFor(t, "the open request to be counted", 5*time.Second, func() bool { return e.gw.Sessions()[up] == 1 })

	release()
	<-done
	waitFor(t, "the count to drop", 5*time.Second, func() bool {
		n, listed := e.gw.Sessions()[up]
		return listed && n == 0
	})
}

// One copy the load balancer would refuse must not take its app out of the
// table, nor keep the others from reaching it.
func TestGateway_InvalidCopyIsLeftOut(t *testing.T) {
	e := newGwEnv(t)
	good := twoCopies(20000, 20001)
	// One address for two copies, and something that is no address.
	bad := Pool{Active: []string{"127.0.0.1:20002", "nonsense"}, Warm: []string{"127.0.0.1:20002", "127.0.0.1:20003"}}
	wantBad := Pool{Active: []string{"127.0.0.1:20002"}, Warm: []string{"127.0.0.1:20003"}}

	var logs syncBuffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	if err := e.gw.SetPool("bad", bad); err != nil {
		t.Fatalf("SetPool(bad) = %v, want the rest to be sent", err)
	}
	if err := e.gw.SetPool("good", good); err != nil {
		t.Fatal(err)
	}
	want := Table{"good": good, "bad": wantBad}
	got, err := Admin{Socket: e.settings.AdminSocket}.Copies()
	if err != nil || !got.Equal(want) {
		t.Errorf("the load balancer has %v, %v; want %v (the app keeps its valid copies)", got, err, want)
	}

	// copies.json holds exactly what was pushed.
	data, err := os.ReadFile(e.settings.CopiesPath)
	var onDisk Table
	if err != nil || json.Unmarshal(data, &onDisk) != nil || !onDisk.Equal(want) {
		t.Errorf("copies.json = %s, %v; want what was pushed", data, err)
	}

	for _, line := range []string{"not sending copy 127.0.0.1:20002 of bad: it is listed twice", "not sending copy nonsense of bad: it is not a host:port address"} {
		if n := strings.Count(logs.String(), line); n != 1 {
			t.Errorf("the log has %q %d times, want once:\n%s", line, n, logs.String())
		}
	}
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Count(l, "of bad") > 1 || strings.Count(l, "bad") > 2 {
			t.Errorf("the line repeats the key: %q", l)
		}
	}

	// Once it is fixed it goes out as it is.
	if err := e.gw.SetPool("bad", twoCopies(20002, 20003)); err != nil {
		t.Fatal(err)
	}
	if got, _ := (Admin{Socket: e.settings.AdminSocket}).Copies(); !got.Equal(Table{"good": good, "bad": twoCopies(20002, 20003)}) {
		t.Errorf("the load balancer has %v after the fix", got)
	}
}
