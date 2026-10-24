package proxy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/process"
)

func TestBackoff(t *testing.T) {
	want := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for n, w := range want {
		if got := backoff(n); got != w {
			t.Errorf("backoff(%d) = %s, want %s", n, got, w)
		}
	}
}

// supEnv is a proxy directory with a Supervisor for it. The proxy is the
// test binary, started as `proxy --config` (see TestMain).
type supEnv struct {
	settings Settings
	dir      string
}

func newSupEnv(t *testing.T) *supEnv {
	t.Helper()
	dir := shortDir(t)
	return &supEnv{settings: SettingsFor(dir), dir: dir}
}

func (e *supEnv) supervisor(t *testing.T, build string) *Supervisor {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if build == "" {
		build = BuildID()
	}
	return &Supervisor{Settings: e.settings, Exe: exe, Build: build, LogPath: filepath.Join(e.dir, "proxy.log")}
}

// run starts s.Run and returns a stop that cancels it and waits for it to
// return, without touching the proxy.
func run(t *testing.T, s *Supervisor) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	var stopped bool
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return stop
}

// cleanupProxy ends whatever proxy the lock in e names, after the test.
func (e *supEnv) cleanupProxy(t *testing.T) {
	t.Cleanup(func() {
		if info, err := readLock(LockPath(e.settings.ConfigPath)); err == nil && info.PID > 0 && lockHeld(LockPath(e.settings.ConfigPath)) {
			syscall.Kill(info.PID, syscall.SIGKILL)
		}
	})
}

func gone(pid int) bool { return syscall.Kill(pid, 0) == syscall.ESRCH }

func TestSupervisor_StartsAndRestartsAfterKill(t *testing.T) {
	e := newSupEnv(t)
	e.cleanupProxy(t)
	s := e.supervisor(t, "")
	var ready atomic.Int32
	s.OnReady = func() { ready.Add(1) }
	run(t, s)

	if !s.WaitReady(10 * time.Second) {
		t.Fatal("the load balancer did not become ready")
	}
	pid1 := s.PID()
	if pid1 == 0 || (Admin{Socket: e.settings.AdminSocket}).Ping() != nil {
		t.Fatalf("pid = %d, ping = %v, want a running load balancer", pid1, (Admin{Socket: e.settings.AdminSocket}).Ping())
	}

	syscall.Kill(pid1, syscall.SIGKILL)

	waitFor(t, "a new load balancer", 3*time.Second, func() bool {
		return s.Running() && s.PID() != pid1 && (Admin{Socket: e.settings.AdminSocket}).Ping() == nil
	})
	if got := ready.Load(); got != 2 {
		t.Errorf("OnReady called %d time(s), want 2", got)
	}
}

func TestSupervisor_AdoptsAfterOwnRestart(t *testing.T) {
	e := newSupEnv(t)
	e.cleanupProxy(t)

	a := e.supervisor(t, "")
	stopA := run(t, a)
	if !a.WaitReady(10 * time.Second) {
		t.Fatal("the load balancer did not become ready")
	}
	pid1 := a.PID()
	stopA() // the proxy stays

	if (Admin{Socket: e.settings.AdminSocket}).Ping() != nil {
		t.Fatal("the load balancer stopped with its supervisor")
	}

	b := e.supervisor(t, "")
	run(t, b)
	if !b.WaitReady(10 * time.Second) {
		t.Fatal("the second supervisor did not become ready")
	}
	if b.PID() != pid1 {
		t.Errorf("adopted pid = %d, want %d", b.PID(), pid1)
	}
	if info, err := readLock(LockPath(e.settings.ConfigPath)); err != nil || info.PID != pid1 {
		t.Errorf("lock = %+v, %v, want it to still name pid %d", info, err, pid1)
	}

	// An adopted proxy is not a child, so its death is noticed by polling.
	syscall.Kill(pid1, syscall.SIGKILL)
	waitFor(t, "a new load balancer", 3*time.Second, func() bool {
		return b.Running() && b.PID() != pid1 && (Admin{Socket: e.settings.AdminSocket}).Ping() == nil
	})
}

func TestSupervisor_ReplacesOtherBuild(t *testing.T) {
	e := newSupEnv(t)
	e.cleanupProxy(t)

	a := e.supervisor(t, "")
	stopA := run(t, a)
	if !a.WaitReady(10 * time.Second) {
		t.Fatal("the load balancer did not become ready")
	}
	pid1 := a.PID()
	stopA()

	b := e.supervisor(t, "other")
	run(t, b)
	if !b.WaitReady(15 * time.Second) {
		t.Fatal("the second supervisor did not become ready")
	}
	if b.PID() == pid1 {
		t.Error("the load balancer from another build was adopted")
	}
	waitFor(t, "the old load balancer to be gone", 5*time.Second, func() bool { return gone(pid1) })
	if info, err := readLock(LockPath(e.settings.ConfigPath)); err != nil || info.PID != b.PID() || info.Build == "other" {
		// The new process records its own BuildID; "other" is only what the
		// supervisor believes it is.
		t.Errorf("lock = %+v, %v, want the new process %d", info, err, b.PID())
	}
}

// startSleep starts a process the tests use as an innocent bystander.
// exited closes when it is gone (a signalled process that nobody waits for
// would linger as a zombie, which still looks alive to kill -0).
func startSleep(t *testing.T) (cmd *exec.Cmd, exited <-chan struct{}) {
	t.Helper()
	cmd = exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep not available")
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-done
	})
	return cmd, done
}

func untouched(t *testing.T, exited <-chan struct{}) {
	t.Helper()
	select {
	case <-exited:
		t.Error("the unrelated process named in the lock was signalled")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestSupervisor_StaleLockFile(t *testing.T) {
	e := newSupEnv(t)
	e.cleanupProxy(t)
	sleeper, exited := startSleep(t)

	// A leftover lock file naming a live process, with nobody holding the lock.
	f, err := acquireLock(LockPath(e.settings.ConfigPath), lockInfo{PID: sleeper.Process.Pid, Build: "old"})
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	s := e.supervisor(t, "")
	run(t, s)
	if !s.WaitReady(10 * time.Second) {
		t.Fatal("the load balancer did not become ready")
	}
	if s.PID() == sleeper.Process.Pid {
		t.Fatal("the supervisor took the stale pid for the load balancer")
	}
	untouched(t, exited)
}

func TestSupervisor_LockHeldByStranger(t *testing.T) {
	e := newSupEnv(t)
	sleeper, exited := startSleep(t)

	// This process holds the lock and the file names a bystander: not a
	// load balancer, so nothing may be signalled.
	f, err := acquireLock(LockPath(e.settings.ConfigPath), lockInfo{PID: sleeper.Process.Pid, Build: "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Short, so that a supervisor that took the holder for a load balancer
	// would have gone on to replace it within the wait below.
	old := startTimeout
	startTimeout = 100 * time.Millisecond
	t.Cleanup(func() { startTimeout = old })

	s := e.supervisor(t, "")
	run(t, s)
	if s.WaitReady(1 * time.Second) {
		t.Fatal("the supervisor started a load balancer although the lock is held")
	}
	untouched(t, exited)
}

// TestSupervisor_AdoptionBringsItUpToDate: an adopted load balancer knows
// nothing of the apps this shinyctl-api manages, so adopting one calls
// OnReady like a start does.
func TestSupervisor_AdoptionBringsItUpToDate(t *testing.T) {
	e := newSupEnv(t)
	e.cleanupProxy(t)

	a := e.supervisor(t, "")
	stopA := run(t, a)
	if !a.WaitReady(10 * time.Second) {
		t.Fatal("the load balancer did not become ready")
	}
	stopA()

	var ready atomic.Int32
	b := e.supervisor(t, "")
	b.OnReady = func() { ready.Add(1) }
	run(t, b)
	waitFor(t, "OnReady after adoption", 10*time.Second, func() bool { return ready.Load() == 1 })
}

// TestSupervisor_ReplacesUnresponsiveBalancer: a process that holds the lock
// and has the load balancer's command line, but doesn't answer on its
// control socket, is stopped and replaced rather than adopted.
func TestSupervisor_ReplacesUnresponsiveBalancer(t *testing.T) {
	e := newSupEnv(t)
	e.cleanupProxy(t)

	// Not a load balancer, but it looks like one in ps and never answers.
	// The shell stays (no exec), so its arguments stay in the command line.
	cmd := exec.Command("sh", "-c", "sleep 60; true", "proxy", "--config", e.settings.ConfigPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skip("sh not available")
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // the shell and its sleep
		<-exited
	})
	waitFor(t, "the stand-in to look like a load balancer", 5*time.Second, func() bool {
		return process.HasCommandLine(cmd.Process.Pid, "proxy --config "+e.settings.ConfigPath)
	})

	f, err := acquireLock(LockPath(e.settings.ConfigPath), lockInfo{PID: cmd.Process.Pid, Build: BuildID()})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	old := startTimeout
	startTimeout = 200 * time.Millisecond
	t.Cleanup(func() { startTimeout = old })

	s := e.supervisor(t, "")
	run(t, s)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the unresponsive process was not stopped")
	}
	if s.Running() && s.PID() == cmd.Process.Pid {
		t.Error("the unresponsive process was adopted")
	}
}

// TestLock_ShorterRecordReplacesLonger: a record shorter than the one before
// it must not leave the end of the old one behind, or the lock can't be read
// and no supervisor adopts or starts a load balancer.
func TestLock_ShorterRecordReplacesLonger(t *testing.T) {
	path := filepath.Join(shortDir(t), "proxy.lock")
	long, err := acquireLock(path, lockInfo{PID: 4242, Build: strings.Repeat("b", 80)})
	if err != nil {
		t.Fatal(err)
	}
	long.Close()

	short, err := acquireLock(path, lockInfo{PID: 7, Build: "b"})
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()

	info, err := readLock(path)
	if err != nil || info.PID != 7 || info.Build != "b" {
		t.Errorf("readLock = %+v, %v, want the short record", info, err)
	}
}
