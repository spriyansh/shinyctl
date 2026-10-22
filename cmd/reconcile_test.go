/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

// TestReconcileLocked_StartFailureRecordsCrash guards the fix for the gap
// where a process that never even started (bad entrypoint, broken
// working-dir, missing Rscript) retried every tick forever with no
// backoff, because only a post-start exit ever reached RecordExit.
func TestReconcileLocked_StartFailureRecordsCrash(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep process.AppLogPath off the real ~/.shinyctl

	dir := t.TempDir()
	entrypoint := filepath.Join(dir, "app.R")
	if err := os.WriteFile(entrypoint, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	spec := manifest.AppSpec{
		AppGroup:       "g",
		AppName:        "n",
		EntrypointFile: entrypoint,
		Port:           18881,
		Replicas:       1,
		AppStateFile:   filepath.Join(dir, "state.json"),
		// A working-dir that doesn't exist makes exec.Cmd.Start fail
		// deterministically and portably, without depending on Rscript
		// being absent or any other OS-specific failure mode.
		WorkingDir: filepath.Join(dir, "does-not-exist"),
	}

	unlock := state.Lock(spec.AppStateFile)
	_, err := reconcileLocked(spec, true, nil)
	unlock()
	if err == nil {
		t.Fatal("reconcileLocked() returned a nil error, want the Start failure")
	}

	got, err := state.Load(spec.AppStateFile)
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Apps) != 1 {
		t.Fatalf("state.Apps = %v, want exactly one entry", got.Apps)
	}

	app := got.Apps[0]
	if app.CrashCount != 1 {
		t.Errorf("CrashCount = %d, want 1", app.CrashCount)
	}
	if !app.LastExitFailed {
		t.Error("LastExitFailed = false, want true")
	}
}

// TestExitWatcher_RecordsCrashOfRecordedProcess guards the other side of
// the PID gate in recordExit: a genuine crash of the recorded process must
// still reach state.json.
func TestExitWatcher_RecordsCrashOfRecordedProcess(t *testing.T) {
	useFakeRscript(t)
	dir := t.TempDir()
	spec := newTestSpec(t, dir, "app", "app.R", freePorts(t, 1)[0])

	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
	})
	a = applyOK(t, spec)
	waitReady(t, spec.Host, spec.Port)

	if !process.IsAlive(spec, a) {
		t.Fatal("started process is not alive")
	}
	if err := syscall.Kill(a.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		app := loadApp(t, spec)
		if app.PID == a.PID && app.CrashCount == 1 && app.LastExitFailed && !app.LastExitAt.IsZero() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("crash not recorded: %+v", app)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestRecordExit_IgnoresReplacedProcess: the exit of a process that a
// re-apply has replaced belongs to no one. Recorded against its successor
// it would be a phantom crash, and the reconcile that follows would act on
// it.
func TestRecordExit_IgnoresReplacedProcess(t *testing.T) {
	useFakeRscript(t)
	dir := t.TempDir()
	ports := freePorts(t, 2)
	spec1 := newTestSpec(t, dir, "app", "app.R", ports[0])

	spec2 := spec1
	spec2.Port = ports[1]
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec2)
		killIfAlive(spec1, a)
	})
	a = applyOK(t, spec1)
	waitReady(t, spec1.Host, spec1.Port)
	b := applyOK(t, spec2)

	if recordExit(spec1, a.PID, process.Exit{At: time.Now(), Failed: true}) {
		t.Error("recordExit() = true for a replaced process, want false")
	}
	got := loadApp(t, spec2)
	if got.PID != b.PID || got.CrashCount != 0 || !got.LastExitAt.IsZero() || got.LastExitFailed {
		t.Errorf("state changed: %+v, want pid %d with no exit recorded", got, b.PID)
	}
}
