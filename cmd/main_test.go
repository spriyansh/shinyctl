/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
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
const fakeRscriptEnv = "SHINYCTL_TEST_FAKE_RSCRIPT"

func TestMain(m *testing.M) {
	if os.Getenv(fakeRscriptEnv) != "" {
		runFakeRscript(strings.Join(os.Args[1:], " "))
	}
	os.Exit(m.Run())
}

var fakeRscriptArgs = regexp.MustCompile(`port=(\d+), host="([^"]+)"`)

// runFakeRscript binds exactly the address process.Start asks for and
// serves until killed. It never returns.
func runFakeRscript(args string) {
	m := fakeRscriptArgs.FindStringSubmatch(args)
	if m == nil {
		os.Stderr.WriteString("fake Rscript: cannot parse port/host from: " + args + "\n")
		os.Exit(1)
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

	// Leak guard: a test binary that dies before cleanup runs must not
	// leave listeners behind.
	ppid := os.Getppid()
	go func() {
		for os.Getppid() == ppid {
			time.Sleep(200 * time.Millisecond)
		}
		os.Exit(0)
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			os.Exit(1)
		}
		c.Close()
	}
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

// freePorts returns n distinct loopback ports. The listeners are held until
// all n are chosen so the OS can't hand the same port out twice.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	var lns []net.Listener
	var ports []int
	for i := 0; i < n; i++ {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		lns = append(lns, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range lns {
		ln.Close()
	}
	return ports
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
	}
}

// applyOK applies spec through the real handler and returns the recorded app.
func applyOK(t *testing.T, spec manifest.AppSpec) state.AppState {
	t.Helper()
	rec := postJSON(t, handleApply, spec)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply %q: status = %d, body = %s", spec.AppName, rec.Code, rec.Body.String())
	}
	return loadApp(t, spec)
}

func loadApp(t *testing.T, spec manifest.AppSpec) state.AppState {
	t.Helper()
	st, err := state.Load(spec.AppStateFile)
	if err != nil {
		t.Fatal(err)
	}
	return st.App()
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
		app := loadApp(t, spec)
		if app.PID != wantPID || app.CrashCount != 0 || !app.LastExitAt.IsZero() {
			t.Fatalf("recorded app = %+v, want pid %d with no exit recorded", app, wantPID)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// killIfAlive is test cleanup: it only signals a PID that still passes the
// identity check, so a reused PID is never hit.
func killIfAlive(spec manifest.AppSpec, app state.AppState) {
	if app.PID > 0 && process.IsAlive(spec, app) {
		syscall.Kill(app.PID, syscall.SIGKILL)
	}
}
