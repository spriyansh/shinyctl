package process

import (
	"fmt"
	"syscall"
	"time"
)

// stopGrace is how long Stop gives a process to exit on SIGTERM before
// escalating to SIGKILL. R honors SIGTERM, but not mid-computation — a
// Shiny session deep in a long calculation can sit on it for a while.
const stopGrace = 10 * time.Second

// killGrace is how long Stop waits after SIGKILL for the process to
// actually be gone — the kill itself is immediate; this covers the reap.
const killGrace = 2 * time.Second

// Stop terminates pid and only returns once it's actually gone: SIGTERM
// first, then SIGKILL if the process is still around after stopGrace. The
// caller's next step is clearing pid's record from state.json, and a
// record cleared while the process is still running (and still holding
// its port) turns the next apply's port check into a phantom crash. A PID
// that's already gone (ESRCH) is treated as success — "make sure it's
// stopped" trivially holds if it already is.
func Stop(pid int) error {
	return stop(pid, stopGrace)
}

// stop is Stop with the SIGTERM grace period as a parameter, for tests.
func stop(pid int, grace time.Duration) error {
	if pid <= 0 {
		return nil
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	if waitGone(pid, grace) {
		return nil
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	if waitGone(pid, killGrace) {
		return nil
	}

	return fmt.Errorf("process %d still running %s after SIGKILL", pid, killGrace)
}

// waitGone polls until pid no longer exists or timeout elapses. A zombie
// still "exists" to Kill(pid, 0); the exit-watcher goroutine's Wait()
// reaps our own children promptly, and init reaps anything we didn't
// start ourselves, so this resolves either way.
func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
