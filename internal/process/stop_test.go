package process

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// startSleeper launches a long-running process for Stop to act on and
// reaps it in the background, the way the exit-watcher goroutine does for
// real apps. ignoreTERM makes it shrug off SIGTERM, so only SIGKILL ends it.
func startSleeper(t *testing.T, ignoreTERM bool) int {
	t.Helper()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found on PATH, skipping")
	}

	script := "exec sleep 30"
	if ignoreTERM {
		// An ignored signal stays ignored across exec, so sleep inherits it.
		script = "trap '' TERM; " + script
	}

	cmd := exec.Command(shPath, "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleeper: %v", err)
	}
	go cmd.Wait()

	t.Cleanup(func() { syscall.Kill(cmd.Process.Pid, syscall.SIGKILL) })

	// Give sh a moment to install the trap and exec; signaling before
	// that would hit a shell that hasn't ignored TERM yet.
	time.Sleep(100 * time.Millisecond)

	return cmd.Process.Pid
}

func TestStop(t *testing.T) {
	t.Run("SIGTERM, and only returns once the process is gone", func(t *testing.T) {
		pid := startSleeper(t, false)

		if err := Stop(pid); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		// No sleep here on purpose: Stop's contract is that the PID is
		// already gone by the time it returns.
		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Errorf("process %d still exists after Stop (err=%v)", pid, err)
		}
	})

	t.Run("escalates to SIGKILL when SIGTERM is ignored", func(t *testing.T) {
		pid := startSleeper(t, true)

		started := time.Now()
		if err := stop(pid, 300*time.Millisecond); err != nil {
			t.Fatalf("stop: %v", err)
		}

		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Errorf("process %d still exists after stop (err=%v)", pid, err)
		}
		if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
			t.Errorf("stop returned after %s, before the %s SIGTERM grace elapsed — did it skip straight to SIGKILL?", elapsed, 300*time.Millisecond)
		}
	})

	t.Run("already-dead PID is not an error", func(t *testing.T) {
		if err := Stop(999999999); err != nil {
			t.Errorf("Stop(dead pid) = %v, want nil", err)
		}
	})

	t.Run("zero/negative PID is a no-op, not an error", func(t *testing.T) {
		if err := Stop(0); err != nil {
			t.Errorf("Stop(0) = %v, want nil", err)
		}
	})
}
