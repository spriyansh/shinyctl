package process

import (
	"fmt"
	"syscall"
	"time"

	"github.com/spriyansh/shinyctl/internal/state"
)

// stopGrace is how long Stop gives a process to exit on SIGTERM before
// escalating to SIGKILL. R honors SIGTERM, but not mid-computation — a
// Shiny session deep in a long calculation can sit on it for a while.
const stopGrace = 10 * time.Second

// killGrace is how long Stop waits after SIGKILL for the process to
// actually be gone — the kill itself is immediate; this covers the reap.
const killGrace = 2 * time.Second

// Stop terminates app's recorded process and only returns once it's
// actually gone: SIGTERM first, then SIGKILL if the process is still around
// after stopGrace. The caller's next step is clearing the record from
// state.json, and a record cleared while the process is still running (and
// still holding its port) turns the next apply's port check into a phantom
// crash. A PID that's already gone (ESRCH) is treated as success — "make
// sure it's stopped" trivially holds if it already is.
//
// It signals only while the process is identifiably the app's own (see
// isOurs; fallbacks are for records that predate the recorded entrypoint),
// and checks again before SIGKILL, because the grace period is long enough
// for a process that exited to have its PID handed to a stranger.
// signaled=false means there was nothing of ours to stop (no PID, already
// gone, or someone else's); that counts as success.
func Stop(app state.AppState, fallbacks ...string) (signaled bool, err error) {
	return stop(app.PID, func() bool { return isOurs(app, fallbacks...) }, stopGrace)
}

// StopIf is Stop for a process that isn't an app's, such as the load
// balancer: SIGTERM, SIGKILL after the same grace period, each signal only
// while ours reports that pid is still the process the caller means.
func StopIf(pid int, ours func() bool) (signaled bool, err error) {
	return stop(pid, ours, stopGrace)
}

// stop is Stop with the identity check and the SIGTERM grace period as
// parameters, for tests.
func stop(pid int, ours func() bool, grace time.Duration) (bool, error) {
	if pid <= 0 || !ours() {
		return false, nil
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return true, nil
		}
		return true, err
	}
	if waitGone(pid, grace) {
		return true, nil
	}
	if !ours() {
		return true, nil
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return true, err
	}
	if waitGone(pid, killGrace) || !ours() {
		return true, nil
	}

	return true, fmt.Errorf("process %d still running %s after SIGKILL", pid, killGrace)
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
