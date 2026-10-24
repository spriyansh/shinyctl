package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/process"
)

// shortDir is a directory short enough for a Unix socket path on macOS
// (t.TempDir bakes the test name into the path).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "px")
	if err != nil {
		t.Fatal(err)
	}
	trackDir(dir)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// freePort returns a loopback port nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// hold listens on 127.0.0.1:port until the test ends.
func hold(t *testing.T, port int) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// upstreamApp is an app on a public port whose copy listens at upstream.
func upstreamApp(key string, publicPort int, upstream string) App {
	return App{Key: key, Name: key, Group: "g", Host: "127.0.0.1", Port: publicPort, Upstreams: []string{upstream}}
}

// startUpstream serves handler on a fresh loopback port.
func startUpstream(t *testing.T, handler http.Handler) (addr string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// serveOn serves handler on the given loopback port, for tests that bring
// an upstream up late.
func serveOn(t *testing.T, port int, handler http.Handler) {
	t.Helper()
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Error(err)
		return
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// waitFor polls cond until it holds or the timeout passes.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s: not true after %s", what, timeout)
}

// refused reports whether nothing accepts connections on host:port.
func refused(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return true
	}
	c.Close()
	return false
}

// syncBuffer is a bytes.Buffer safe to write from a subprocess's copier
// goroutine while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// subproxy is a proxy process started from the test binary.
type subproxy struct {
	cmd    *exec.Cmd
	stderr *syncBuffer
	exited chan struct{}
	err    error // valid once exited is closed
}

// startSubproxy writes cfg to dir/config.json and starts the test binary as
// `proxy --config` in homeDir (HOME), without waiting for it to answer.
func startSubproxy(t *testing.T, dir, homeDir string, cfg []byte) *subproxy {
	t.Helper()
	path := SettingsFor(dir).ConfigPath
	if err := os.WriteFile(path, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	return startSubproxyAt(t, path, homeDir)
}

func startSubproxyAt(t *testing.T, configPath, homeDir string) *subproxy {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &subproxy{stderr: &syncBuffer{}, exited: make(chan struct{})}
	p.cmd = exec.Command(exe, "proxy", "--config", configPath)
	p.cmd.Env = append(os.Environ(), "HOME="+homeDir, "XDG_DATA_HOME="+homeDir, "XDG_CONFIG_HOME="+homeDir)
	p.cmd.Stderr = p.stderr
	p.cmd.Stdout = p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			p.cmd.Process.Kill()
			<-p.exited
		}
	})
	return p
}

func (p *subproxy) pid() int { return p.cmd.Process.Pid }

func (p *subproxy) signal(sig syscall.Signal) { p.cmd.Process.Signal(sig) }

// waitAnswering waits for the control socket to answer, failing early if the
// process exits first.
func (p *subproxy) waitAnswering(t *testing.T, a Admin) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.exited:
			t.Fatalf("proxy exited before answering: %v\nstderr: %s", p.err, p.stderr.String())
		default:
		}
		if a.Ping() == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("proxy did not answer within 10s\nstderr: %s", p.stderr.String())
}

// get fetches url and returns status and body. A connection that fails
// (EOF, reset) is tried again a few times: the load balancer can drop a
// connection that arrives in the few milliseconds of a reload, which the
// tests that reload right before a request would otherwise trip over.
func get(t *testing.T, url string) (int, string, http.Header) {
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
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// dirs created by shortDir, for the interrupt handler in TestMain.
var (
	trackedMu   sync.Mutex
	trackedDirs []string
)

func trackDir(dir string) {
	trackedMu.Lock()
	defer trackedMu.Unlock()
	trackedDirs = append(trackedDirs, dir)
}

// reapTracked ends any load balancer still running from a tracked dir, and
// removes the dirs. Supervisors start theirs in a session of their own, so
// an interrupted run would leave them behind.
func reapTracked() {
	trackedMu.Lock()
	defer trackedMu.Unlock()
	for _, dir := range trackedDirs {
		for _, lock := range []string{
			filepath.Join(dir, "proxy.lock"),
			filepath.Join(dir, ".shinyctl", "proxy", "proxy.lock"),
		} {
			if !lockHeld(lock) {
				continue
			}
			if info, err := readLock(lock); err == nil && info.PID > 0 && process.HasCommandLine(info.PID, "proxy --config") {
				syscall.Kill(info.PID, syscall.SIGKILL)
			}
		}
		os.RemoveAll(dir)
	}
}
