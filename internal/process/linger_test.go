package process

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A logind that does not answer must not hold shinyctl-api's start up.
func TestLingerWarning_DoesNotWaitForASlowLoginctl(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 20\n"
	if err := os.WriteFile(filepath.Join(dir, "loginctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	if got := LingerWarning(); got != "" {
		t.Errorf("LingerWarning = %q, want nothing when loginctl does not answer", got)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v, want a few seconds at most", took)
	}
}
