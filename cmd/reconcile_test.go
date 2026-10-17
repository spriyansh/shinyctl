/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

// TestReconcileSpec_StartFailureRecordsCrash guards the fix for the gap
// where a process that never even started (bad entrypoint, broken
// working-dir, missing Rscript) retried every tick forever with no
// backoff, because only a post-start exit ever reached RecordExit.
func TestReconcileSpec_StartFailureRecordsCrash(t *testing.T) {
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

	if _, err := reconcileSpec(spec, true); err == nil {
		t.Fatal("reconcileSpec() returned a nil error, want the Start failure")
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
