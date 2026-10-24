/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// waitGone waits until pid no longer exists (reaped by whoever waits on it).
func waitGone(t *testing.T, app state.AppState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for process.PIDRunning(app) {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d still exists after SIGKILL", app.PID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReconcile_ExitPendingIsLeftToItsWatcher: a tick that finds a process
// gone before its exit-watcher recorded the exit (the watcher is waiting for
// the lock the tick holds) must not start the app again. The test holds the
// lock itself, kills the process, and runs the tick's cycle by hand, which
// puts the tick first, every time.
func TestReconcile_ExitPendingIsLeftToItsWatcher(t *testing.T) {
	for _, policy := range []manifest.RestartPolicy{manifest.RestartNever, manifest.RestartAlways} {
		t.Run(string(policy), func(t *testing.T) {
			useFakeRscript(t)
			spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
			spec.RestartPolicy = policy
			var a state.AppState
			t.Cleanup(func() {
				postJSON(t, handleDelete, spec)
				killIfAlive(spec, a)
			})
			a = applyOK(t, spec)
			waitServed(t, spec, a.PID)

			unlock := state.Lock(spec.AppStateFile)
			if err := syscall.Kill(a.PID, syscall.SIGKILL); err != nil {
				unlock()
				t.Fatal(err)
			}
			waitGone(t, a)

			resp, err := reconcileLocked(spec, false, nil)
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			if resp.Action != reconcile.ActionNone {
				t.Fatalf("the tick answered %q while the exit was pending, want none", resp.Action)
			}

			// The watcher now records the exit with its real status.
			deadline := time.Now().Add(5 * time.Second)
			var got state.AppState
			for {
				got = loadApp(t, spec)
				if got.CrashCount == 1 && got.LastExitFailed {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("exit not recorded: %+v", got)
				}
				time.Sleep(20 * time.Millisecond)
			}

			if policy == manifest.RestartNever {
				time.Sleep(300 * time.Millisecond)
				if again := loadApp(t, spec); again.PID != a.PID {
					t.Errorf("a restart-policy never app was started again: pid %d -> %d", a.PID, again.PID)
				}
				waitClosed(t, spec.Host, spec.Port)
			}
		})
	}
}

// TestReconcile_UnseenExitIsCounted: a process adopted from an earlier
// daemon has no watcher, so the tick is the only one to notice it died. If
// this daemon saw it alive, the tick counts the exit, as a failure, before
// deciding.
func TestReconcile_UnseenExitIsCounted(t *testing.T) {
	// Under always the app is backing off after the crash it now counts.
	for _, policy := range []manifest.RestartPolicy{manifest.RestartNever, manifest.RestartAlways} {
		t.Run(string(policy), func(t *testing.T) {
			useFakeRscript(t)
			spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
			spec.RestartPolicy = policy
			var a state.AppState
			t.Cleanup(func() {
				postJSON(t, handleDelete, spec)
				killIfAlive(spec, a)
			})
			a = startCopy(t, spec)
			waitServed(t, spec, a.PID)

			// One tick while it is alive: this daemon has now seen it.
			reconcileApp(spec.AppStateFile)

			if err := syscall.Kill(a.PID, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			waitGone(t, a)

			started := reconcileApp(spec.AppStateFile)
			got := loadApp(t, spec)
			if started || got.PID != a.PID {
				t.Errorf("started = %v, pid %d -> %d, want no start", started, a.PID, got.PID)
			}
			if got.CrashCount != 1 || !got.LastExitFailed || got.LastExitAt.Before(got.StartedAt) {
				t.Errorf("recorded app = %+v, want one failed exit recorded", got)
			}

			// A second tick doesn't count it again.
			reconcileApp(spec.AppStateFile)
			if again := loadApp(t, spec); again.CrashCount != 1 {
				t.Errorf("crash count = %d after a second tick, want 1", again.CrashCount)
			}
		})
	}
}

// deadRecord saves the record of a process that ended while no daemon was
// looking: it started, and is gone, and no exit was recorded.
func deadRecord(t *testing.T, spec manifest.AppSpec) state.AppState {
	t.Helper()
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	a, _ := startRaw(t, spec)
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{a}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(a.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitGone(t, a)
	return a
}

// TestReconcile_AfterARebootEveryAppStarts: after a reboot (or any time no
// daemon was running) the records name processes that are gone and that this
// daemon never saw alive. That is not a crash: nothing is counted, a
// restart-policy never app starts like any other, as it always has.
func TestReconcile_AfterARebootEveryAppStarts(t *testing.T) {
	useFakeRscript(t)
	ports := freePorts(t, 2)
	always := newTestSpec(t, t.TempDir(), "always", "app.R", ports[0])
	never := newTestSpec(t, t.TempDir(), "never", "app.R", ports[1])
	never.RestartPolicy = manifest.RestartNever
	specs := map[string]manifest.AppSpec{"always": always, "never": never}

	old := map[string]state.AppState{}
	started := map[string]state.AppState{}
	t.Cleanup(func() {
		for name, spec := range specs {
			postJSON(t, handleDelete, spec)
			killIfAlive(spec, old[name])
			killIfAlive(spec, started[name])
		}
	})
	for name, spec := range specs {
		old[name] = deadRecord(t, spec)
		forgetSeen(spec.AppStateFile) // a fresh daemon has seen nothing
	}
	// What an earlier process of the same app left behind doesn't count as
	// having seen this one (a reused PID, or another start).
	markSeen(always.AppStateFile, state.AppState{PID: 1, StartedAt: time.Unix(1, 0)})

	for name, spec := range specs {
		if !reconcileApp(spec.AppStateFile) {
			t.Errorf("%s: the tick did not start the app", name)
		}
		got := loadApp(t, spec)
		started[name] = got
		if got.PID == old[name].PID || got.CrashCount != 0 || !got.LastExitAt.IsZero() {
			t.Errorf("%s: recorded app = %+v, want a new process and no crash counted", name, got)
		}
	}
	if err := syncProxy(""); err != nil { // what the tick does after a start
		t.Fatal(err)
	}
	for name, spec := range specs {
		waitServed(t, spec, started[name].PID)
	}
}

// TestReconcile_CrashIsFollowedThroughToTheRestart: a crashed app comes back
// on a later tick once its backoff is over. The exit-watcher's PID has to
// leave the watched set when it is done, or every tick after the crash
// would wait for an exit that is already recorded.
func TestReconcile_CrashIsFollowedThroughToTheRestart(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var a, b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
		killIfAlive(spec, b)
	})
	a = applyOK(t, spec)
	waitServed(t, spec, a.PID)

	if err := syscall.Kill(a.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for loadApp(t, spec).CrashCount != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the exit was not recorded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFor := time.Now().Add(10 * time.Second)
	for {
		reconcileApp(spec.AppStateFile) // what a tick does; the first backoff is 2 s
		if b = loadApp(t, spec); b.PID != a.PID && b.PID > 0 {
			break
		}
		if time.Now().After(waitFor) {
			t.Fatalf("the crashed app was not started again: %+v", b)
		}
		time.Sleep(250 * time.Millisecond)
	}
	waitServed(t, spec, b.PID)
}

// TestReconcile_ApplyDoesNotWaitForAPendingExit: apply and restart mean "run
// it now". They don't defer to an exit that is still to be recorded, as a
// tick does.
func TestReconcile_ApplyDoesNotWaitForAPendingExit(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var a, b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
		killIfAlive(spec, b)
	})
	a = applyOK(t, spec)
	waitServed(t, spec, a.PID)

	unlock := state.Lock(spec.AppStateFile)
	if err := syscall.Kill(a.PID, syscall.SIGKILL); err != nil {
		unlock()
		t.Fatal(err)
	}
	waitGone(t, a)
	resp, err := reconcileLocked(spec, true, nil)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	b = loadApp(t, spec)
	if resp.Action != reconcile.ActionStart || b.PID == a.PID {
		t.Errorf("action = %q, recorded %+v, want a start with a new process", resp.Action, b)
	}
}

// TestSeen_DroppedWhenTheAppIsDeleted: what the daemon remembers about seeing
// a process alive is one entry per app and goes with the app.
func TestSeen_DroppedWhenTheAppIsDeleted(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var a state.AppState
	t.Cleanup(func() { killIfAlive(spec, a) })
	a = applyOK(t, spec)
	waitServed(t, spec, a.PID)

	reconcileApp(spec.AppStateFile)
	if !wasSeen(spec.AppStateFile, a) {
		t.Fatal("a tick that found the process alive did not remember it")
	}
	if rec := postJSON(t, handleDelete, spec); rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if wasSeen(spec.AppStateFile, a) {
		t.Error("the entry survived the delete")
	}
}
