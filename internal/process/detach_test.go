package process

import (
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLaunchDetached(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found on PATH, skipping")
	}

	logPath := filepath.Join(t.TempDir(), "detached.log")

	pid, err := LaunchDetached(sleepPath, []string{"5"}, logPath)
	if err != nil {
		t.Fatalf("LaunchDetached: %v", err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)

	time.Sleep(100 * time.Millisecond)

	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("process %d not running shortly after launch: %v", pid, err)
	}
}
