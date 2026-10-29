package proxy

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spriyansh/shinyctl/internal/atomicfile"
	"github.com/spriyansh/shinyctl/internal/process"
)

// Supervisor keeps the load balancer's process running for shinyctl-api: it
// starts it, adopts one a previous shinyctl-api left running, and starts it
// again when it dies. It never stops it on its own shutdown: the apps stay
// reachable while shinyctl-api is down or restarting, and the next
// shinyctl-api adopts the process.
type Supervisor struct {
	Settings Settings
	Exe      string // the shinyctl binary to start as `proxy --config`
	Build    string // BuildID of Exe
	LogPath  string // the proxy's stdout and stderr (api.log)

	// OnReady is called, with no lock held, each time the load balancer
	// becomes available (started or adopted), so shinyctl-api can bring it
	// up to date.
	OnReady func()

	// OnHandedOver is called, with no lock held, when the load balancer that
	// has just become available took over from another that was running (a
	// handover between builds), so shinyctl-api can act on what it had to
	// wait for at once and not at its next tick.
	OnHandedOver func()

	// ensuring is true from Expect, and while ensure runs, until the load
	// balancer is running: the load balancer is being started or replaced.
	ensuring   atomic.Bool
	handedOver atomic.Bool

	mu         sync.Mutex
	running    bool
	pid        int
	generation int
	ready      chan struct{} // closed while running
}

// startTimeout is how long a new proxy gets to answer on its control
// socket, and how long an existing one gets to do the same before it is
// declared unresponsive and replaced. A variable so tests can shorten it.
var startTimeout = 5 * time.Second

const (
	// healthyRun is how long a proxy has to run for its eventual death to
	// start the restart backoff over.
	healthyRun = 60 * time.Second

	backoffMax = 30 * time.Second
)

// backoff is the wait before the nth consecutive restart attempt: none for
// the first, then 1 s, doubling to 30 s.
func backoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	d := time.Second
	for i := 1; i < n; i++ {
		d *= 2
		if d >= backoffMax {
			return backoffMax
		}
	}
	return d
}

// child is a proxy process this Supervisor started.
type child struct {
	cmd  *exec.Cmd
	done chan struct{} // closed when the process has exited
	err  error         // its exit status, valid once done is closed
}

// Run supervises until ctx is done, which it does without touching the
// proxy.
func (s *Supervisor) Run(ctx context.Context) {
	restarts := 0
	for {
		s.ensuring.Store(true)
		pid, c, err := s.ensure()
		if err != nil {
			s.ensuring.Store(false)
			log.Printf("proxy: could not start the load balancer: %v; retrying in %s", err, backoff(restarts))
			if !sleep(ctx, backoff(restarts)) {
				return
			}
			restarts++
			continue
		}

		s.setRunning(pid)
		s.ensuring.Store(false)
		started := time.Now()
		if s.OnReady != nil {
			s.OnReady()
		}
		if s.handedOver.Swap(false) && s.OnHandedOver != nil {
			go s.OnHandedOver()
		}

		how := s.watch(ctx, pid, c)
		if ctx.Err() != nil {
			return
		}
		s.setNotRunning()
		log.Printf("proxy: the load balancer (pid %d) %s; restarting it", pid, how)

		if time.Since(started) >= healthyRun {
			restarts = 0
		}
		if !sleep(ctx, backoff(restarts)) {
			return
		}
		restarts++
	}
}

// sleep waits d, and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d == 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// watch blocks until the proxy is gone or ctx is done, and says how it
// went. A child is waited on; an adopted proxy is not ours to wait on, so
// its lock is polled: the kernel drops it when the process dies.
func (s *Supervisor) watch(ctx context.Context, pid int, c *child) string {
	if c != nil {
		select {
		case <-ctx.Done():
			return ""
		case <-c.done:
			return fmt.Sprintf("exited (%v)", c.err)
		}
	}

	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-tick.C:
			if !lockHeld(LockPath(s.Settings.ConfigPath)) {
				return "is gone"
			}
		}
	}
}

// ensure leaves exactly one proxy running and answering, and returns its
// pid, plus the child if this call started it.
func (s *Supervisor) ensure() (int, *child, error) {
	lock := LockPath(s.Settings.ConfigPath)
	admin := Admin{Socket: s.Settings.AdminSocket}

	// A handover that an earlier shinyctl-api started and that is still going
	// on finishes by itself; this one waits for it instead of starting a
	// third load balancer.
	s.settleHandover(admin, lock)

	if lockHeld(lock) {
		info := waitHolder(lock)
		ours := func() bool { return info.PID > 0 && process.HasCommandLine(info.PID, s.commandLine()) }
		if !ours() {
			// Never signalled: it is not demonstrably our proxy.
			return 0, nil, fmt.Errorf("the load balancer's lock %s is held by another process (pid %d)", lock, info.PID)
		}

		switch {
		case !waitPing(admin, startTimeout):
			log.Printf("proxy: the load balancer (pid %d) does not answer; replacing it", info.PID)
		case info.Build != s.Build:
			log.Printf("proxy: replacing the load balancer (pid %d), which another shinyctl build started", info.PID)
			pid, c, err := s.startAlongside(admin, info.PID)
			if err == nil {
				return pid, c, nil
			}
			log.Printf("proxy: could not start the new load balancer next to the old one (%v); stopping the old one first", err)
			if h := readHolderPID(lock); h > 0 && h != info.PID {
				// Another load balancer replaced it meanwhile: look again.
				return s.ensure()
			}
		default:
			log.Printf("proxy: adopted the running load balancer (pid %d)", info.PID)
			return info.PID, nil, nil
		}

		if _, err := process.StopIf(info.PID, ours); err != nil {
			return 0, nil, fmt.Errorf("stopping the load balancer (pid %d): %w", info.PID, err)
		}
	}

	return s.start(admin)
}

// settleHandover waits while the control socket is answered by a load
// balancer that was started to replace the one holding the lock (--after its
// pid): that one stops the old one and takes the lock by itself, and in the
// moment between the two the lock is free, which must not look like "no load
// balancer". It returns when the lock has moved to the one that answers, when
// what answers is not such a successor, or after handoverWait.
func (s *Supervisor) settleHandover(admin Admin, lock string) {
	deadline := time.Now().Add(handoverWait())
	for time.Now().Before(deadline) {
		next, err := admin.ServingPID()
		if err != nil || next <= 0 || readHolderPID(lock) == next ||
			!process.HasCommandLine(next, s.commandLine()+" "+afterFlag+" ") {
			return
		}
		s.handedOver.Store(true)
		time.Sleep(50 * time.Millisecond)
	}
	log.Printf("proxy: a handover between load balancers did not finish in %s", handoverWait())
}

// HandoverWait is how long a handover may take, for callers that wait for it.
func HandoverWait() time.Duration { return handoverWait() }

// handoverWait is how long a handover may take: the new load balancer's grace,
// the old one's stop (SIGTERM, then SIGKILL) and a margin. A variable so a
// test can shorten it.
var handoverWait = func() time.Duration {
	return handoverGrace + process.StopWait + startTimeout
}

// startAlongside starts a load balancer next to the running one, old, and
// waits for the handover. The new one serves every port too (both listen with
// SO_REUSEPORT), says so on the control socket, and after handoverGrace stops
// old itself (SIGTERM, SIGKILL after the usual grace) and takes the lock; this
// only watches, so the handover also finishes if shinyctl-api goes away in the
// middle. The old one is stopped with SIGTERM, answers the requests in flight, and leaves the control socket as the new one's.
func (s *Supervisor) startAlongside(admin Admin, old int) (int, *child, error) {
	cmd, err := process.StartDetached(s.Exe, []string{"proxy", "--config", s.Settings.ConfigPath, afterFlag, strconv.Itoa(old)}, s.LogPath)
	if err != nil {
		return 0, nil, err
	}
	c := &child{cmd: cmd, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	pid := cmd.Process.Pid
	fail := func(err error) (int, *child, error) {
		cmd.Process.Kill()
		<-c.done
		return 0, nil, err
	}

	deadline := time.Now().Add(startTimeout)
	for {
		select {
		case <-c.done:
			return 0, nil, fmt.Errorf("it exited as it started (%v)", c.err)
		default:
		}
		if got, err := admin.ServingPID(); err == nil && got == pid {
			break
		}
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("it did not answer within %s", startTimeout))
		}
		time.Sleep(20 * time.Millisecond)
	}

	lock := LockPath(s.Settings.ConfigPath)
	for deadline := time.Now().Add(handoverWait()); readHolderPID(lock) != pid; {
		select {
		case <-c.done:
			return 0, nil, fmt.Errorf("it exited (%v)", c.err)
		default:
		}
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("it did not take the lock within %s", handoverWait()))
		}
		time.Sleep(20 * time.Millisecond)
	}
	log.Printf("proxy: started the load balancer (pid %d) next to the old one (pid %d), which it then stopped", pid, old)
	s.handedOver.Store(true)
	return pid, c, nil
}

// afterFlag is the new load balancer's flag naming the one it replaces; a
// variable so a test can make the new one exit, to see the fall back.
var afterFlag = "--after"

func (s *Supervisor) commandLine() string {
	return "proxy --config " + s.Settings.ConfigPath
}

// waitPing polls the control socket until it answers or timeout passes.
func waitPing(a Admin, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if a.Ping() == nil {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// start launches a proxy and waits for its control socket to answer. It
// loads the last good settings, or an empty bootstrap on a first run.
func (s *Supervisor) start(admin Admin) (int, *child, error) {
	if _, err := os.Stat(s.Settings.ConfigPath); os.IsNotExist(err) {
		boot, err := Render(s.Settings, nil)
		if err != nil {
			return 0, nil, err
		}
		if err := atomicfile.Write(s.Settings.ConfigPath, boot); err != nil {
			return 0, nil, err
		}
	}

	cmd, err := process.StartDetached(s.Exe, []string{"proxy", "--config", s.Settings.ConfigPath}, s.LogPath)
	if err != nil {
		return 0, nil, err
	}
	c := &child{cmd: cmd, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()

	deadline := time.Now().Add(startTimeout)
	for {
		select {
		case <-c.done:
			return 0, nil, fmt.Errorf("the load balancer exited as it started (%v); see shinyctl logs --api", c.err)
		default:
		}
		// Ready is when the control socket names this process as serving
		// (lb.MarkServing, after the settings loaded or the fallback without
		// apps did), not when it first answers: while a load fails, the
		// control socket of that load answers for a moment before it is torn
		// down, and a push at that instant meets an EOF.
		if got, err := admin.ServingPID(); err == nil && got == cmd.Process.Pid {
			log.Printf("proxy: started the load balancer (pid %d)", cmd.Process.Pid)
			return cmd.Process.Pid, c, nil
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			<-c.done
			return 0, nil, fmt.Errorf("the load balancer did not answer within %s; see shinyctl logs --api", startTimeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (s *Supervisor) readyLocked() chan struct{} {
	if s.ready == nil {
		s.ready = make(chan struct{})
	}
	return s.ready
}

func (s *Supervisor) setRunning(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := s.readyLocked()
	s.running, s.pid = true, pid
	s.generation++
	close(ch)
}

func (s *Supervisor) setNotRunning() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.ready = make(chan struct{})
}

// Expect says a load balancer is about to be started, before Run has got as
// far as saying so itself.
func (s *Supervisor) Expect() { s.ensuring.Store(true) }

// Starting reports that the load balancer is being started, or replaced by
// one of another build: not running yet, but on its way.
func (s *Supervisor) Starting() bool { return s.ensuring.Load() && !s.Running() }

// WaitReady blocks until the load balancer is running, or timeout passes.
func (s *Supervisor) WaitReady(timeout time.Duration) bool {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return true
	}
	ch := s.readyLocked()
	s.mu.Unlock()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

// Running reports whether the load balancer is up.
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// PID is the running load balancer's pid, or 0.
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return 0
	}
	return s.pid
}

// Generation counts how many times the load balancer has become available,
// so a caller can tell that it is not the process it last talked to.
func (s *Supervisor) Generation() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// Stop ends the current load balancer and waits until it is gone. shinyctl
// never calls it; tests do, after cancelling Run, so no process outlives
// them.
func (s *Supervisor) Stop() error {
	pid := s.PID()
	if pid == 0 {
		return nil
	}
	_, err := process.StopIf(pid, func() bool { return process.HasCommandLine(pid, s.commandLine()) })
	return err
}
