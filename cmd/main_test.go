/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/state"
)

// fakeRscriptEnv switches this test binary into a stand-in for Rscript:
// useFakeRscript symlinks the binary as "Rscript" on PATH, so the daemon
// code under test launches real processes that bind the requested
// host:port, without R being installed.
//
// Tests that launch these processes never replace the appRegistry global.
// Exit-watcher goroutines read it after the request that started them has
// returned, and nothing orders those reads before a later swap, so a swap
// would be a data race. They use the package's in-memory instance, key each
// app by their own temp state file, and clean up through handleDelete,
// which forgets the app.
// testAdmin talks to the test load balancer's control socket, to look at what
// it holds rather than at what the gateway believes.
var testAdmin proxy.Admin

const fakeRscriptEnv = "SHINYCTL_TEST_FAKE_RSCRIPT"

// TestMain runs the whole package against one real load balancer: the test
// binary itself, started as `proxy --config` the way shinyctl-api starts the
// real one (the check for it comes first, because a load balancer restarted
// during a test inherits the fake-Rscript variable). It lives in a short
// temp dir, since Unix socket paths are limited to 103 bytes on macOS, and
// HOME points there too, so nothing here can reach the real ~/.shinyctl.
// testParentEnv names the test binary that started a test load balancer.
const testParentEnv = "SHINYCTL_TEST_PARENT"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "proxy" {
		if v := os.Getenv(testParentEnv); v != "" {
			// A load balancer of the tests exits when the test binary that
			// started it is gone. The real one must outlive shinyctl-api and
			// does not look at this.
			want, _ := strconv.Atoi(v)
			go func() {
				for os.Getppid() == want {
					time.Sleep(200 * time.Millisecond)
				}
				os.Exit(0)
			}()
		}
		rootCmd.SetArgs(os.Args[1:])
		if err := rootCmd.Execute(); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(fakeRscriptEnv) != "" {
		runFakeRscript(strings.Join(os.Args[1:], " "))
	}
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	// The machine may run other daemons, and other test runs, that pick
	// internal ports too: these tests scan in an area of their own, by pid.
	defaultPortStart = func() int { return 20000 + (os.Getpid()*131)%9000 }
	// The load balancer started below leaves with this process, whatever
	// ends it (a panic, a timeout): a test-only guard, see TestMain.
	os.Setenv(testParentEnv, strconv.Itoa(os.Getpid()))
	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	for _, k := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME"} {
		os.Setenv(k, dir)
	}

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	proxyLog := filepath.Join(dir, "proxy.log")
	g, sup, err := newGateway(dir, exe, proxyLog)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	gateway = g
	testAdmin = proxy.Admin{Socket: proxy.SettingsFor(dir).AdminSocket}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sup.Run(ctx)
		close(done)
	}()

	// The load balancer runs in a session of its own, so a Ctrl-C at the
	// terminal doesn't reach it: without this it would outlive the run, with
	// its temp dir.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	go func() {
		<-sigs
		cancel()
		<-done
		sup.Stop()
		os.RemoveAll(dir)
		os.Exit(130)
	}()
	if !sup.WaitReady(15 * time.Second) {
		cancel()
		<-done
		sup.Stop()
		fmt.Fprintf(os.Stderr, "the load balancer did not start; its log:\n%s\n", readTail(proxyLog))
		return 1
	}

	code := m.Run()

	cancel()
	<-done
	sup.Stop()
	if code != 0 {
		fmt.Fprintf(os.Stderr, "load balancer log:\n%s\n", readTail(proxyLog))
	}
	return code
}

// readTail returns the end of a log, for a failed run.
func readTail(path string) string {
	data, _ := os.ReadFile(path)
	if len(data) > 4000 {
		data = data[len(data)-4000:]
	}
	return string(data)
}

var fakeRscriptArgs = regexp.MustCompile(`port=(\d+), host="([^"]+)"`)

// runFakeRscript binds exactly the address process.Start asks for and
// serves until killed. It never returns.
func runFakeRscript(args string) {
	// Leak guard: a test binary that dies before cleanup runs must not leave
	// copies behind. The parent is read first of all, before the start-up
	// delay: a copy orphaned during the delay would otherwise take its new
	// parent (init) for the one to watch, and never leave.
	ppid := os.Getppid()
	go func() {
		for os.Getppid() == ppid {
			time.Sleep(200 * time.Millisecond)
		}
		os.Exit(0)
	}()

	m := fakeRscriptArgs.FindStringSubmatch(args)
	if m == nil {
		os.Stderr.WriteString("fake Rscript: cannot parse port/host from: " + args + "\n")
		os.Exit(1)
	}

	// FAKE_RSCRIPT_EXIT makes it die at once, the way a broken app does, and
	// FAKE_RSCRIPT_DELAY makes it slow to start, the way a big app is.
	if os.Getenv("FAKE_RSCRIPT_EXIT") == "1" {
		os.Stderr.WriteString("fake Rscript: asked to exit\n")
		os.Exit(1)
	}
	if d, err := time.ParseDuration(os.Getenv("FAKE_RSCRIPT_DELAY")); err == nil {
		time.Sleep(d)
	}

	network := "tcp4"
	if net.ParseIP(m[2]).To4() == nil {
		network = "tcp6"
	}

	ln, err := net.Listen(network, net.JoinHostPort(m[2], m[1]))
	if err != nil {
		os.Stderr.WriteString("fake Rscript: " + err.Error() + "\n")
		os.Exit(1)
	}

	// Answers with its own pid, so a test can tell which process a request
	// reached through the load balancer. /hold keeps the request open until
	// the client goes: a session, as far as the load balancer can tell.
	body := fmt.Sprintf("pid=%d", os.Getpid())
	http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hold" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.WriteString(w, body)
	}))
	os.Exit(1)
}

// useFakeRscript puts a fake Rscript first on PATH and gives the test its
// own HOME, so logs stay off the real ~/.shinyctl.
func useFakeRscript(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(bin, "Rscript")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(fakeRscriptEnv, "1")
	t.Setenv("HOME", t.TempDir())
}

// The range this package's tests take ports from: below the range the
// system hands out to outgoing connections (32768 and up on Linux, 49152 and
// up on macOS), so no other program's connection can take a port between the
// probe and the test's own bind, and above the ports shinyctl keeps for the
// apps' own processes (20000 to 29999).
const (
	portLo = 31500
	portHi = 32767
)

var (
	testPortMu   sync.Mutex
	testPortNext = portLo + rand.New(rand.NewSource(time.Now().UnixNano()^int64(os.Getpid()))).Intn(portHi-portLo+1)
)

// freePorts returns n distinct ports of the range that nothing listens on,
// on loopback or on every address, because tests bind either.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	testPortMu.Lock()
	defer testPortMu.Unlock()
	var ports []int
	for tries := 0; len(ports) < n; tries++ {
		if tries > 2*(portHi-portLo+1) {
			t.Fatalf("no free port between %d and %d", portLo, portHi)
		}
		p := testPortNext
		if testPortNext++; testPortNext > portHi {
			testPortNext = portLo
		}
		if portFree(p) {
			ports = append(ports, p)
		}
	}
	return ports
}

func portFree(p int) bool {
	for _, host := range []string{"127.0.0.1", "0.0.0.0"} {
		ln, err := net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(p)))
		if err != nil {
			return false
		}
		ln.Close()
	}
	return true
}

func newTestSpec(t *testing.T, dir, name, entrypoint string, port int) manifest.AppSpec {
	t.Helper()
	ep := filepath.Join(dir, entrypoint)
	if err := os.WriteFile(ep, nil, 0644); err != nil {
		t.Fatal(err)
	}
	return manifest.AppSpec{
		AppGroup:       "g",
		AppName:        name,
		EntrypointFile: ep,
		Host:           "127.0.0.1",
		Port:           port,
		AppStateFile:   filepath.Join(dir, "state.json"),
		WorkingDir:     dir,

		// One copy, as before copies existed: tests that want a warm spare
		// say so.
		WarmReplicas: intPtr(0),
	}
}

func intPtr(n int) *int { return &n }

// applyOK applies spec through the real handler and returns the recorded app.
func applyOK(t *testing.T, spec manifest.AppSpec) state.AppState {
	t.Helper()
	rec := postJSON(t, handleApply, spec)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply %q: status = %d, body = %s", spec.AppName, rec.Code, rec.Body.String())
	}
	return loadApp(t, spec)
}

// loadApp returns the record of copy 1, the only copy of an app with one.
func loadApp(t *testing.T, spec manifest.AppSpec) state.AppState {
	t.Helper()
	st, err := state.Load(spec.AppStateFile)
	if err != nil {
		t.Fatal(err)
	}
	app, _ := st.Copy(1)
	return app
}

// loadCopies returns the record of every copy, in copy order.
func loadCopies(t *testing.T, spec manifest.AppSpec) []state.AppState {
	t.Helper()
	st, err := state.Load(spec.AppStateFile)
	if err != nil {
		t.Fatal(err)
	}
	return st.Copies()
}

func waitReady(t *testing.T, host string, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if process.IsReady(host, port) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s:%d not ready after 5s", host, port)
}

// assertNoExitRecorded watches the state file for a short while and fails
// if the recorded process changes or any exit is recorded against it.
func assertNoExitRecorded(t *testing.T, spec manifest.AppSpec, wantPID int) {
	t.Helper()
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		var app state.AppState
		for _, rec := range loadCopies(t, spec) {
			if rec.PID == wantPID {
				app = rec
			}
		}
		if app.PID != wantPID || app.CrashCount != 0 || !app.LastExitAt.IsZero() {
			t.Fatalf("recorded app = %+v, want pid %d with no exit recorded", app, wantPID)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// newestApp returns the record of the newest copy: after a redeploy, the one
// that runs the new version.
func newestApp(t *testing.T, spec manifest.AppSpec) state.AppState {
	t.Helper()
	var newest state.AppState
	for _, rec := range loadCopies(t, spec) {
		if rec.StartedAt.After(newest.StartedAt) {
			newest = rec
		}
	}
	return newest
}

// killIfAlive is test cleanup: it only signals a PID that still passes the
// identity check, so a reused PID is never hit.
func killIfAlive(spec manifest.AppSpec, app state.AppState) {
	if app.PID > 0 && process.IsAlive(spec, app) {
		syscall.Kill(app.PID, syscall.SIGKILL)
	}
}

// startCopy starts spec's process the way the daemon would and records it,
// without a reconcile: an app adopted from a previous daemon, whose load
// balancer kept serving it. It is registered, given an internal port, and
// synced into the load balancer.
func startCopy(t *testing.T, spec manifest.AppSpec) state.AppState {
	t.Helper()
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	app, _ := startRaw(t, spec, 1)
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{app}}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, process.InternalHost, app.Port)
	refreshPools()
	if err := syncProxy(spec.AppStateFile); err != nil {
		t.Fatalf("syncProxy: %v", err)
	}
	return app
}

// startRaw starts copy n of spec's app on a free internal port and returns
// its record and exit channel, without registering, saving or syncing
// anything.
func startRaw(t *testing.T, spec manifest.AppSpec, n int) (state.AppState, <-chan process.Exit) {
	t.Helper()
	port, err := process.PickPort(takenPorts(spec.AppStateFile, n), func(p int) bool {
		return process.PortAvailable(process.InternalHost, p) == nil
	}, currentHooks().PortStart())
	if err != nil {
		t.Fatal(err)
	}
	app, done, err := process.Start(spec, n, port, 0)
	if err != nil {
		t.Fatal(err)
	}
	return app, done
}

// startOldStyle starts spec's process the way a build before the load
// balancer did: on the app's own port, with a record that has no copy
// number and no started-from.
func startOldStyle(t *testing.T, spec manifest.AppSpec) state.AppState {
	t.Helper()
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	app, _, err := process.Start(spec, 1, spec.Port, 0)
	if err != nil {
		t.Fatal(err)
	}
	app.Copy = 0
	app.StartedFrom = nil
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{app}}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, spec.Host, spec.Port)
	return app
}

// dialAddr is the address to connect to for a listener on host:port.
func dialAddr(host string, port int) string {
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// get fetches url and returns the status and body; a connection error is a
// failure.
func get(t *testing.T, url string) (int, string) {
	t.Helper()
	var resp *http.Response
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		resp, err = (&http.Client{Timeout: 20 * time.Second}).Get(url)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// servedBody fetches the app's public address once, returning the body or
// "" when it can't be reached.
func servedBody(host string, port int) string {
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Get("http://" + dialAddr(host, port) + "/")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return string(body)
}

// waitServed waits until a request to the app's public address reaches
// process pid.
//
// The process is waited for on its internal port first, so the first request
// through the load balancer finds it listening and the tests don't depend on
// how long a request waits for a copy that is still starting.
func waitServed(t *testing.T, spec manifest.AppSpec, pid int) {
	t.Helper()
	if app := loadApp(t, spec); app.PID == pid && app.Copy >= 1 {
		waitReady(t, process.InternalHost, app.Port)
	}
	want := fmt.Sprintf("pid=%d", pid)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if servedBody(spec.EffectiveHost(), spec.Port) == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s:%d does not serve %s after 10s (got %q)", spec.EffectiveHost(), spec.Port, want, servedBody(spec.EffectiveHost(), spec.Port))
}

// waitClosed waits until nothing accepts connections on host:port.
func waitClosed(t *testing.T, host string, port int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !process.IsReady(host, port) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s:%d still accepts connections after 5s", host, port)
}
