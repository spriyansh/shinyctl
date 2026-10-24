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

// adoptCopies starts copies 1 and 2 of spec without a daemon's watchers, as
// an earlier daemon would have left them, and records them.
func adoptCopies(t *testing.T, spec manifest.AppSpec) (one, two state.AppState) {
	t.Helper()
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	// Each is recorded and waited for before the next picks its port.
	one, _ = startRaw(t, spec, 1)
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{one}}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, process.InternalHost, one.Port)
	two, _ = startRaw(t, spec, 2)
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{one, two}}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, process.InternalHost, two.Port)
	refreshPools()
	if err := syncProxy(spec.AppStateFile); err != nil {
		t.Fatal(err)
	}
	return one, two
}

// TestReconcile_ExitPendingOfCopy2: a copy whose exit is still to be
// recorded is left to its watcher without holding up the others.
func TestReconcile_ExitPendingOfCopy2(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	warm := copyByDial(t, spec, pool.Warm[0])
	active := copyByDial(t, spec, pool.Active[0])

	unlock := state.Lock(spec.AppStateFile)
	if err := syscall.Kill(warm.PID, syscall.SIGKILL); err != nil {
		unlock()
		t.Fatal(err)
	}
	waitGone(t, warm)
	resp, err := reconcileLocked(spec, reconcileOpts{})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Action != reconcile.ActionNone {
		t.Errorf("the tick answered %q while the exit was pending, want none", resp.Action)
	}

	waitUntil(t, "the exit to be recorded", 5*time.Second, func() bool {
		rec, _ := loadCopyRecord(spec, warm.Copy)
		return rec.CrashCount == 1 && rec.LastExitFailed
	})
	if rec, _ := loadCopyRecord(spec, active.Copy); rec.CrashCount != 0 || rec.PID != active.PID {
		t.Errorf("the other copy = %+v, want it untouched", rec)
	}
}

// TestReconcile_UnseenExitOfCopy2: only the copy that died is counted.
func TestReconcile_UnseenExitOfCopy2(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	one, two := adoptCopies(t, spec)

	reconcileApp(spec.AppStateFile) // this daemon sees both alive
	if err := syscall.Kill(two.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitGone(t, two)

	if reconcileApp(spec.AppStateFile) {
		t.Error("a copy still inside its backoff was started")
	}
	got1, _ := loadCopyRecord(spec, 1)
	got2, _ := loadCopyRecord(spec, 2)
	if got2.CrashCount != 1 || !got2.LastExitFailed || got2.PID != two.PID {
		t.Errorf("copy 2 = %+v, want one failed exit recorded", got2)
	}
	if got1.CrashCount != 0 || got1.PID != one.PID {
		t.Errorf("copy 1 = %+v, want no exit recorded", got1)
	}

	reconcileApp(spec.AppStateFile)
	if again, _ := loadCopyRecord(spec, 2); again.CrashCount != 1 {
		t.Errorf("crash count = %d after a second tick, want 1", again.CrashCount)
	}
}

// TestReconcile_AfterARebootEveryCopyStarts: both copies of an app are gone
// and were never seen alive by this daemon: they just start, with nothing
// counted, even under a restart policy that gives up on a crash.
func TestReconcile_AfterARebootEveryCopyStarts(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	spec.RestartPolicy = manifest.RestartNever
	cleanupApp(t, spec)
	one, two := adoptCopies(t, spec)
	for _, rec := range []state.AppState{one, two} {
		syscall.Kill(rec.PID, syscall.SIGKILL)
		waitGone(t, rec)
		forgetSeen(spec.AppStateFile, rec.Copy) // a fresh daemon has seen nothing
	}

	if !reconcileApp(spec.AppStateFile) {
		t.Fatal("the tick did not start the copies")
	}
	for _, old := range []state.AppState{one, two} {
		got, _ := loadCopyRecord(spec, old.Copy)
		if got.PID == old.PID || got.PID == 0 || got.CrashCount != 0 || !got.LastExitAt.IsZero() {
			t.Errorf("copy %d = %+v, want a new process and no crash counted", old.Copy, got)
		}
	}
	waitRoles(t, spec, 1, 1)
}

func TestSeen_DroppedForEveryCopyWhenTheAppIsDeleted(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	adoptCopies(t, spec)

	reconcileApp(spec.AppStateFile)
	recs := loadCopies(t, spec) // as saved: a time read from the file has no monotonic reading
	for _, rec := range recs {
		if !wasSeen(spec.AppStateFile, rec) {
			t.Fatalf("a tick that found copy %d alive did not remember it", rec.Copy)
		}
	}
	if rec := postJSON(t, handleDelete, spec); rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, rec := range recs {
		if wasSeen(spec.AppStateFile, rec) {
			t.Errorf("the entry of copy %d survived the delete", rec.Copy)
		}
	}
}

// TestRecordExit_IgnoresOtherCopies: an exit belongs to the copy it
// happened to, whatever pid another copy has.
func TestRecordExit_IgnoresOtherCopies(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	one, two := adoptCopies(t, spec)

	if recordExit(spec, 2, one.PID, process.Exit{At: time.Now(), Failed: true}) {
		t.Error("recordExit() = true for copy 2 with copy 1's pid")
	}
	if !recordExit(spec, 2, two.PID, process.Exit{At: time.Now(), Failed: true}) {
		t.Error("recordExit() = false for copy 2's own pid")
	}
	got1, _ := loadCopyRecord(spec, 1)
	got2, _ := loadCopyRecord(spec, 2)
	if got1.CrashCount != 0 || got2.CrashCount != 1 {
		t.Errorf("crash counts = %d and %d, want 0 and 1", got1.CrashCount, got2.CrashCount)
	}
}
