package process

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// Raw-PID kill is safe: nothing reaps this child while the test holds
	// it, so its PID cannot be reused.
	defer syscall.Kill(pid, syscall.SIGKILL)

	time.Sleep(100 * time.Millisecond)

	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("process %d not running shortly after launch: %v", pid, err)
	}
}

func TestStartDetached(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found on PATH, skipping")
	}

	t.Run("output is appended to the log, and Wait reports the exit", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "detached.log")
		if err := os.WriteFile(logPath, []byte("earlier\n"), 0644); err != nil {
			t.Fatal(err)
		}

		cmd, err := StartDetached(shPath, []string{"-c", "echo out; echo err >&2; exit 3"}, logPath)
		if err != nil {
			t.Fatalf("StartDetached: %v", err)
		}
		var exitErr *exec.ExitError
		if err := cmd.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
			t.Fatalf("Wait = %v, want exit status 3", err)
		}

		data, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"earlier\n", "out\n", "err\n"} {
			if !strings.Contains(string(data), want) {
				t.Errorf("log = %q, want it to contain %q", data, want)
			}
		}
	})

	t.Run("the child is in a session of its own", func(t *testing.T) {
		cmd, err := StartDetached(shPath, []string{"-c", "exec sleep 5"}, filepath.Join(t.TempDir(), "d.log"))
		if err != nil {
			t.Fatalf("StartDetached: %v", err)
		}
		defer func() {
			cmd.Process.Kill()
			cmd.Wait()
		}()

		// A new session makes the child the leader of a process group of its
		// own, where it would otherwise share ours.
		ours, err := syscall.Getpgid(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		theirs, err := syscall.Getpgid(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if theirs == ours {
			t.Errorf("the child's process group is this process's (%d)", ours)
		}
		if theirs != cmd.Process.Pid {
			t.Errorf("the child's process group = %d, want its own pid %d", theirs, cmd.Process.Pid)
		}
	})
}
