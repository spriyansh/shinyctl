package process

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestParseRSS(t *testing.T) {
	// macOS pads with leading spaces; procps right-aligns the columns.
	out := []byte("  123  2048\n45678 10\n\nnot a line\n  7 notanumber\n  9 1 extra\n 55 300")

	want := map[int]int64{123: 2048 * 1024, 45678: 10 * 1024, 55: 300 * 1024}
	if got := parseRSS(out); !reflect.DeepEqual(got, want) {
		t.Errorf("parseRSS() = %v, want %v", got, want)
	}

	if got := parseRSS([]byte("  123  2048\n"))[123]; got != 2<<20 {
		t.Errorf("123 -> %d bytes, want 2 MiB", got)
	}
}

func TestRSS_OnePSCall(t *testing.T) {
	var calls [][]string
	orig := psOutput
	t.Cleanup(func() { psOutput = orig })
	psOutput = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("1 100\n2 200\n3 300\n"), nil
	}

	got := RSS([]int{1, 2, 3})
	if len(calls) != 1 || !strings.Contains(strings.Join(calls[0], " "), "-p 1,2,3") {
		t.Errorf("ps calls = %v, want exactly one with -p 1,2,3", calls)
	}
	if got[2] != 200*1024 {
		t.Errorf("RSS()[2] = %d, want %d", got[2], 200*1024)
	}

	calls = nil
	if got := RSS(nil); got != nil || len(calls) != 0 {
		t.Errorf("RSS(nil) = %v after %d ps calls, want nil and none", got, len(calls))
	}
}

// TestRSS_RealProcesses runs the real ps, which is the part that differs
// between macOS and Linux: the comma-separated -p list and the KiB unit.
func TestRSS_RealProcesses(t *testing.T) {
	var pids []int
	for i := 0; i < 2; i++ {
		c := exec.Command("sleep", "30")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Process.Kill(); c.Wait() })
		pids = append(pids, c.Process.Pid)
	}

	// A PID that existed a moment ago: macOS ps rejects an out-of-range
	// PID outright, which is not the case under test.
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	gone := exited.Process.Pid
	got := RSS(append(pids, gone))

	for _, pid := range pids {
		if got[pid] <= 0 {
			t.Errorf("RSS()[%d] = %d, want a positive size", pid, got[pid])
		}
	}
	if _, ok := got[gone]; ok {
		t.Errorf("RSS() has an entry for the nonexistent pid %d", gone)
	}
}
