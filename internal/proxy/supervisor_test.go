package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy/lb"
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
	return &supEnv{settings: settingsFor(dir), dir: dir}
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

// servingEnv is a proxy directory whose settings already serve an app, so a
// load balancer started by a supervisor listens on a port a test can use.
func (e *supEnv) servingApp(t *testing.T) App {
	t.Helper()
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "up") }))
	app := servedApp(t, "a", freePort(t), up)
	cfg, err := Render(e.settings, []App{app})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.settings.ConfigPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	copies, err := json.Marshal(lb.Current())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.settings.CopiesPath, copies, 0600); err != nil {
		t.Fatal(err)
	}
	return app
}

// A load balancer another build started is replaced by one that serves next
// to it before the old one goes, so no connection is refused in between.
func TestSupervisor_ReplacesAnotherBuildWithoutAGap(t *testing.T) {
	e := newSupEnv(t)
	e.cleanupProxy(t)
	app := e.servingApp(t)

	a := e.supervisor(t, "")
	stopA := run(t, a)
	if !a.WaitReady(10 * time.Second) {
		t.Fatal("the load balancer did not become ready")
	}
	pid1 := a.PID()
	stopA()
	if status, body, _ := get(t, appURL(app)); status != 200 || body != "up" {
		t.Fatalf("the app answers %d %q before the replacement, want 200 up", status, body)
	}

	logs := captureLog(t)
	stop := hammer(appURL(app))
	b := e.supervisor(t, "other")
	var handedOver atomic.Int32
	b.OnHandedOver = func() { handedOver.Add(1) }
	b.Expect()
	if !b.Starting() {
		t.Error("the supervisor does not say the load balancer is starting")
	}
	run(t, b)
	if !b.WaitReady(15 * time.Second) {
		t.Fatal("the second supervisor did not become ready")
	}
	time.Sleep(300 * time.Millisecond)
	ok, failed := stop()
	if b.Starting() {
		t.Error("the supervisor still says the load balancer is starting")
	}
	waitFor(t, "the end of the handover to be announced", 5*time.Second, func() bool { return handedOver.Load() == 1 })

	if b.PID() == pid1 {
		t.Fatal("the load balancer from another build was adopted")
	}
	// The old one answers the requests it is in the middle of before it goes.
	// On Linux the kernel keeps hashing new connections to the old socket
	// until it closes, so a request that reaches it in that instant can still
	// fail once (1 handover in 15 had two, with 4 clients); macOS hands all new
	// connections to the newest socket.
	allowed := 0
	if runtime.GOOS == "linux" {
		allowed = 2
	}
	if failed > allowed || ok == 0 {
		t.Errorf("%d requests answered and %d failed while the load balancer was replaced, want at most %d failed", ok, failed, allowed)
	}
	waitFor(t, "the old load balancer to be gone", 5*time.Second, func() bool { return gone(pid1) })
	if info, err := readLock(LockPath(e.settings.ConfigPath)); err != nil || info.PID != b.PID() {
		t.Errorf("lock = %+v, %v, want the new process %d", info, err, b.PID())
	}
	admin := Admin{Socket: e.settings.AdminSocket}
	if err := admin.Ping(); err != nil {
		t.Errorf("the control socket does not answer after the replacement: %v", err)
	}
	// The control socket is the new one's, and a reload goes through it.
	if pid, err := admin.ServingPID(); err != nil || pid != b.PID() {
		t.Errorf("the control socket names pid %d (%v), want the new one (%d)", pid, err, b.PID())
	}
	if cfg, err := os.ReadFile(e.settings.ConfigPath); err != nil {
		t.Error(err)
	} else if err := admin.Load(cfg); err != nil {
		t.Errorf("a reload through the control socket after the replacement: %v", err)
	}
	if !strings.Contains(logs.String(), "next to the old one") || !strings.Contains(logs.String(), "which it then stopped") {
		t.Errorf("log = %q, want the line about starting next to the old one", logs.String())
	}
}

// If the new load balancer can't start next to the old one, the old one is
// stopped first and a new one started, as before.
func TestSupervisor_FallsBackWhenTheNewOneCannotServe(t *testing.T) {
	e := newSupEnv(t)
	e.cleanupProxy(t)

	a := e.supervisor(t, "")
	stopA := run(t, a)
	if !a.WaitReady(10 * time.Second) {
		t.Fatal("the load balancer did not become ready")
	}
	pid1 := a.PID()
	stopA()

	// A flag the test binary's proxy doesn't read: the new one then starts
	// as an ordinary start, which the lock refuses, and it exits.
	orig := afterFlag
	afterFlag = "--no-such-flag"
	t.Cleanup(func() { afterFlag = orig })
	logs := captureLog(t)

	b := e.supervisor(t, "other")
	run(t, b)
	if !b.WaitReady(20 * time.Second) {
		t.Fatal("the second supervisor did not become ready")
	}
	if b.PID() == pid1 {
		t.Error("the load balancer from another build was adopted")
	}
	waitFor(t, "the old load balancer to be gone", 5*time.Second, func() bool { return gone(pid1) })
	if err := (Admin{Socket: e.settings.AdminSocket}).Ping(); err != nil {
		t.Errorf("no load balancer answers: %v", err)
	}
	if info, err := readLock(LockPath(e.settings.ConfigPath)); err != nil || info.PID != b.PID() {
		t.Errorf("lock = %+v, %v, want the new process %d", info, err, b.PID())
	}
	if !strings.Contains(logs.String(), "could not start the new load balancer next to the old one") {
		t.Errorf("log = %q, want the line about the fall back", logs.String())
	}
}

// captureLog sends the standard logger's output to the returned buffer until
// the test ends.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

// startAlongside returns only once the lock has moved to the new load
// balancer, which waits handoverGrace before it stops the old one.
func TestSupervisor_StartAlongsideWaitsForTheLock(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_HANDOVER_GRACE", "1500ms")
	e := newSupEnv(t)
	e.cleanupProxy(t)
	e.servingApp(t)

	a := e.supervisor(t, "")
	stopA := run(t, a)
	if !a.WaitReady(10 * time.Second) {
		t.Fatal("the load balancer did not become ready")
	}
	old := a.PID()
	stopA()

	b := e.supervisor(t, "other")
	start := time.Now()
	pid, _, err := b.startAlongside(Admin{Socket: e.settings.AdminSocket}, old)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	if got := readHolderPID(LockPath(e.settings.ConfigPath)); got != pid {
		t.Errorf("startAlongside returned with the lock held by %d, want the new load balancer (%d)", got, pid)
	}
	if took := time.Since(start); took < 1400*time.Millisecond {
		t.Errorf("startAlongside took %s, less than the grace period the new load balancer waits", took)
	}
	if !gone(old) {
		t.Error("the old load balancer is still running")
	}
}

// loadBalancers counts the processes running as the load balancer for this
// config, so a test can see that there is never a third.
func (e *supEnv) loadBalancers(t *testing.T) []int {
	t.Helper()
	out, err := exec.Command("ps", "-ax", "-o", "pid=,command=").Output()
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) > 3 && strings.Contains(line, "proxy --config "+e.settings.ConfigPath) {
			if pid, err := strconv.Atoi(f[0]); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

// A shinyctl-api that goes away right after starting the new load balancer
// leaves the handover to the new one, which finishes it by itself. The
// shinyctl-api that comes next, while it goes on, waits for it and adopts the
// new load balancer (the same build), or hands over from it (another build):
// there is never a third load balancer, and one serves in the end.
func TestSupervisor_ADyingShinyctlAPILeavesNoThirdLoadBalancer(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build string // the next shinyctl-api's build; "" is the new load balancer's own
		adopt bool
	}{
		{"the same build adopts", "", true},
		{"another build hands over", "newer", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHINYCTL_TEST_HANDOVER_GRACE", "1500ms")
			e := newSupEnv(t)
			e.cleanupProxy(t)
			app := e.servingApp(t)
			admin := Admin{Socket: e.settings.AdminSocket}

			home := e.dir
			a := startSubproxyAt(t, e.settings.ConfigPath, home)
			a.waitAnswering(t, admin)
			// What the dying shinyctl-api left behind: the new load balancer.
			b := startSubproxyArgs(t, e.settings.ConfigPath, home, "--after", fmt.Sprint(a.pid()))
			waitFor(t, "the new one to say it serves", 10*time.Second, func() bool {
				pid, _ := admin.ServingPID()
				return pid == b.pid()
			})
			if got := readHolderPID(LockPath(e.settings.ConfigPath)); got != a.pid() {
				t.Fatalf("the handover has finished already (lock held by %d)", got)
			}

			// The next shinyctl-api, mid-handover.
			next := e.supervisor(t, tc.build)
			var handedOver atomic.Int32
			next.OnHandedOver = func() { handedOver.Add(1) }
			stop := hammer(appURL(app))
			run(t, next)
			if !next.WaitReady(30 * time.Second) {
				t.Fatal("the next shinyctl-api did not get a load balancer")
			}
			time.Sleep(300 * time.Millisecond)
			ok, failed := stop()
			waitFor(t, "the end of the handover to be announced", 5*time.Second, func() bool { return handedOver.Load() == 1 })
			if tc.adopt && next.PID() != b.pid() {
				t.Errorf("the next shinyctl-api uses pid %d, want to adopt the new load balancer (%d)", next.PID(), b.pid())
			}
			if !tc.adopt && next.PID() == b.pid() {
				t.Errorf("the next shinyctl-api adopted a load balancer of another build")
			}
			select {
			case <-a.exited:
			case <-time.After(10 * time.Second):
				t.Error("the first load balancer is still running")
			}
			if !tc.adopt {
				waitFor(t, "the second to be handed over from", 15*time.Second, func() bool { return gone(b.pid()) })
			}
			if lbs := e.loadBalancers(t); len(lbs) != 1 || lbs[0] != next.PID() {
				t.Errorf("load balancers running: %v, want just %d", lbs, next.PID())
			}
			if got := readHolderPID(LockPath(e.settings.ConfigPath)); got != next.PID() {
				t.Errorf("lock held by %d, want %d", got, next.PID())
			}
			allowed := 0
			if runtime.GOOS == "linux" {
				allowed = 3
			}
			if failed > allowed || ok == 0 {
				t.Errorf("%d requests answered, %d failed during the handover, want at most %d failed", ok, failed, allowed)
			}
			if err := admin.Ping(); err != nil {
				t.Errorf("the control socket does not answer: %v", err)
			}
		})
	}
}

// A handover that never finishes does not hold shinyctl-api for ever: it waits
// as long as a handover may take, says so, and goes on with the usual decision.
func TestSupervisor_SettleGivesUpOnAHandoverThatNeverFinishes(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_HANDOVER_GRACE", "60s") // the new one never gets to stop the old one
	e := newSupEnv(t)
	e.cleanupProxy(t)
	e.servingApp(t)
	admin := Admin{Socket: e.settings.AdminSocket}
	a := startSubproxyAt(t, e.settings.ConfigPath, e.dir)
	a.waitAnswering(t, admin)
	b := startSubproxyArgs(t, e.settings.ConfigPath, e.dir, "--after", fmt.Sprint(a.pid()))
	waitFor(t, "the new one to say it serves", 10*time.Second, func() bool {
		pid, _ := admin.ServingPID()
		return pid == b.pid()
	})

	orig := handoverWait
	handoverWait = func() time.Duration { return 800 * time.Millisecond }
	t.Cleanup(func() { handoverWait = orig })
	logs := captureLog(t)
	start := time.Now()
	e.supervisor(t, "").settleHandover(admin, LockPath(e.settings.ConfigPath))
	if took := time.Since(start); took < 700*time.Millisecond || took > 3*time.Second {
		t.Errorf("settleHandover took %s, want about the 800ms a handover may take", took)
	}
	if !strings.Contains(logs.String(), "did not finish") {
		t.Errorf("log = %q, want it to say the handover did not finish", logs.String())
	}
	if got := readHolderPID(LockPath(e.settings.ConfigPath)); got != a.pid() {
		t.Errorf("lock held by %d, want it untouched (%d)", got, a.pid())
	}
}

// A load balancer whose settings fail to load serves no apps instead. While
// the failed load is torn down its control socket answers for a moment, and
// that is not the load balancer being up: the supervisor calls it started only
// once it says it serves, and a request to the control socket right after
// never meets the teardown.
func TestSupervisor_AFailedLoadIsNotStartedUntilTheFallbackServes(t *testing.T) {
	// The control socket is up well before the load balancer says it serves.
	t.Setenv("SHINYCTL_TEST_SERVE_DELAY", "300ms")
	for i := 0; i < 3; i++ {
		e := newSupEnv(t)
		e.cleanupProxy(t)
		taken := freePort(t)
		hold(t, taken)
		cfg, err := Render(e.settings, []App{plainApp("a", taken)})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.settings.ConfigPath, cfg, 0o600); err != nil {
			t.Fatal(err)
		}
		s := e.supervisor(t, "")
		stop := run(t, s)
		if !s.WaitReady(15 * time.Second) {
			t.Fatal("the load balancer did not become ready")
		}
		admin := Admin{Socket: e.settings.AdminSocket}
		if pid, err := admin.ServingPID(); err != nil || pid != s.PID() {
			t.Fatalf("round %d: the control socket names %d (%v) when ready, want %d", i, pid, err, s.PID())
		}
		if _, err := admin.Config(); err != nil {
			t.Fatalf("round %d: a request right after ready: %v", i, err)
		}
		stop()
		if pid := readHolderPID(LockPath(e.settings.ConfigPath)); pid > 0 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
