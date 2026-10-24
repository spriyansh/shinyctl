package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/state"
)

// sleeperApp is the record of a sleeper started by startSleeper, whose
// command line contains "sleep".
func sleeperApp(pid int, entrypoint string) state.AppState {
	return state.AppState{PID: pid, StartedAt: time.Now(), EntrypointFile: entrypoint}
}

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

	// Killed by handle, so a reused PID can never be hit.
	t.Cleanup(func() { cmd.Process.Kill() })

	// Wait until sh has installed the trap and exec'd sleep; signaling
	// before that would hit a shell that hasn't ignored TERM yet. Prefix
	// match: the sh -c command line also contains "sleep 30".
	pid := cmd.Process.Pid
	deadline := time.Now().Add(2 * time.Second)
	for {
		out, _ := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
		if strings.HasPrefix(strings.TrimSpace(string(out)), "sleep") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sleeper never exec'd sleep")
		}
		time.Sleep(10 * time.Millisecond)
	}

	return pid
}

func TestStop(t *testing.T) {
	t.Run("SIGTERM, and only returns once the process is gone", func(t *testing.T) {
		pid := startSleeper(t, false)

		if signaled, err := Stop(sleeperApp(pid, "sleep")); err != nil || !signaled {
			t.Fatalf("Stop = (%v, %v), want (true, nil)", signaled, err)
		}

		// No sleep here on purpose: Stop's contract is that the PID is
		// already gone by the time it returns.
		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Errorf("process %d still exists after Stop (err=%v)", pid, err)
		}
	})

	t.Run("escalates to SIGKILL when SIGTERM is ignored", func(t *testing.T) {
		pid := startSleeper(t, true)

		app := sleeperApp(pid, "sleep")
		started := time.Now()
		if signaled, err := stop(pid, func() bool { return isOurs(app) }, 300*time.Millisecond); err != nil || !signaled {
			t.Fatalf("stop = (%v, %v), want (true, nil)", signaled, err)
		}

		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Errorf("process %d still exists after stop (err=%v)", pid, err)
		}
		if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
			t.Errorf("stop returned after %s, before the %s SIGTERM grace elapsed — did it skip straight to SIGKILL?", elapsed, 300*time.Millisecond)
		}
	})

	t.Run("already-dead PID is not an error", func(t *testing.T) {
		if signaled, err := Stop(sleeperApp(999999999, "sleep")); err != nil || signaled {
			t.Errorf("Stop(dead pid) = (%v, %v), want (false, nil)", signaled, err)
		}
	})

	t.Run("zero/negative PID is a no-op, not an error", func(t *testing.T) {
		if signaled, err := Stop(state.AppState{}); err != nil || signaled {
			t.Errorf("Stop(zero) = (%v, %v), want (false, nil)", signaled, err)
		}
	})

	t.Run("a process running something else is left alone", func(t *testing.T) {
		pid := startSleeper(t, false)

		if signaled, err := Stop(sleeperApp(pid, "some-other-app.R")); err != nil || signaled {
			t.Fatalf("Stop = (%v, %v), want (false, nil)", signaled, err)
		}
		if err := syscall.Kill(pid, 0); err != nil {
			t.Errorf("process %d was signaled: %v", pid, err)
		}
	})

	t.Run("re-checks identity before SIGKILL", func(t *testing.T) {
		pid := startSleeper(t, true)

		// Ours when the signal is sent, a stranger by the time the grace
		// period ends: the PID changed hands in between.
		calls := 0
		ours := func() bool {
			calls++
			return calls == 1
		}
		if signaled, err := stop(pid, ours, 300*time.Millisecond); err != nil || !signaled {
			t.Fatalf("stop = (%v, %v), want (true, nil)", signaled, err)
		}
		if err := syscall.Kill(pid, 0); err != nil {
			t.Errorf("process %d was killed after losing identity: %v", pid, err)
		}
		if calls < 2 {
			t.Errorf("ours called %d time(s), want at least 2", calls)
		}
	})
}

func TestStopIf(t *testing.T) {
	t.Run("refuses when the process is not the one meant", func(t *testing.T) {
		pid := startSleeper(t, false)

		if signaled, err := StopIf(pid, func() bool { return false }); err != nil || signaled {
			t.Fatalf("StopIf = (%v, %v), want (false, nil)", signaled, err)
		}
		if err := syscall.Kill(pid, 0); err != nil {
			t.Errorf("process %d was signaled: %v", pid, err)
		}
	})

	t.Run("stops the process it is told is its own", func(t *testing.T) {
		pid := startSleeper(t, false)

		if signaled, err := StopIf(pid, func() bool { return true }); err != nil || !signaled {
			t.Fatalf("StopIf = (%v, %v), want (true, nil)", signaled, err)
		}
		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Errorf("process %d still exists after StopIf (err=%v)", pid, err)
		}
	})
}

func TestProxyDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	dir, err := ProxyDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(os.Getenv("HOME"), ".shinyctl", "proxy"); dir != want {
		t.Errorf("ProxyDir = %q, want %q", dir, want)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0700 {
		t.Errorf("proxy dir = %v, %v, want a 0700 directory", fi, err)
	}
}
