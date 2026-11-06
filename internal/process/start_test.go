package process

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

// A copy leads a process group of its own, so that what it forks can be
// signalled with it. The real Start runs here, with a shell script named
// Rscript first on PATH that stands in for R.
func TestStart_OwnProcessGroup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)

	bin := t.TempDir()
	shim := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "Rscript"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	spec := manifest.AppSpec{AppGroup: "g", AppName: "own-group", EntrypointFile: "app.R", WorkingDir: t.TempDir()}
	app, done, err := Start(spec, 1, 20123, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The group, not just the process: a stray leftover would otherwise
	// outlive the test.
	t.Cleanup(func() {
		syscall.Kill(-app.PID, syscall.SIGKILL)
		<-done
	})

	pgid, err := syscall.Getpgid(app.PID)
	if err != nil {
		t.Fatal(err)
	}
	if pgid != app.PID {
		t.Errorf("the copy's process group = %d, want its own pid %d", pgid, app.PID)
	}
	if own := syscall.Getpgrp(); pgid == own {
		t.Errorf("the copy is in the test's own process group %d", own)
	}
}
