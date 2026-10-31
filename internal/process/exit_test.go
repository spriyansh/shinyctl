package process

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestExitOf: a process killed by a signal reports the signal, and one that
// ends by itself reports none, failed or not.
func TestExitOf(t *testing.T) {
	cases := []struct {
		name       string
		script     string
		wantFailed bool
		wantSignal syscall.Signal
	}{
		{"killed", "kill -9 $$", true, syscall.SIGKILL},
		{"terminated", "kill -15 $$", true, syscall.SIGTERM},
		{"failed", "exit 3", true, 0},
		{"clean", "exit 0", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", c.script)
			cmd.Run() // a failed exit is the point
			exit := exitOf(cmd.ProcessState)
			if exit.Failed != c.wantFailed || exit.Signal != c.wantSignal {
				t.Errorf("exitOf = failed %v, signal %v; want failed %v, signal %v", exit.Failed, exit.Signal, c.wantFailed, c.wantSignal)
			}
			if exit.Reason != "" || exit.At.IsZero() {
				t.Errorf("exitOf = reason %q, at %v; want no reason and a time", exit.Reason, exit.At)
			}
		})
	}
}
