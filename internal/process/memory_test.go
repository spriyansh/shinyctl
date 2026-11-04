package process

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeMeasure replaces the platform's measurement and ps, and records the
// calls to both.
type fakeMeasure struct {
	measured [][][]int
	psCalls  [][]string
}

func stubMeasure(t *testing.T, got map[int]int64, err error, ps string) *fakeMeasure {
	t.Helper()
	f := &fakeMeasure{}
	origMeasure, origPS := measureTrees, psOutput
	t.Cleanup(func() { measureTrees, psOutput = origMeasure, origPS })
	measureTrees = func(trees [][]int) (map[int]int64, error) {
		f.measured = append(f.measured, trees)
		return got, err
	}
	psOutput = func(args ...string) ([]byte, error) {
		f.psCalls = append(f.psCalls, args)
		return []byte(ps), nil
	}
	return f
}

func TestMemory_PlatformMeasuresAll(t *testing.T) {
	f := stubMeasure(t, map[int]int64{1: 100, 2: 200}, nil, "1 1\n2 2\n")

	got, err := Memory([]int{1, 2})
	if err != nil {
		t.Errorf("Memory() error = %v, want none", err)
	}
	if want := (map[int]int64{1: 100, 2: 200}); !reflect.DeepEqual(got, want) {
		t.Errorf("Memory() = %v, want %v", got, want)
	}
	if len(f.psCalls) != 0 {
		t.Errorf("ps calls = %v, want none", f.psCalls)
	}
}

func TestMemory_PsStandsInForTheMissingOne(t *testing.T) {
	f := stubMeasure(t, map[int]int64{1: 100}, nil, "2 3\n")

	got, err := Memory([]int{1, 2})
	if want := (map[int]int64{1: 100, 2: 3 * 1024}); !reflect.DeepEqual(got, want) {
		t.Errorf("Memory() = %v, want %v", got, want)
	}
	if len(f.psCalls) != 1 || f.psCalls[0][len(f.psCalls[0])-1] != "2" {
		t.Errorf("ps calls = %v, want exactly one for pid 2 alone", f.psCalls)
	}
	if err == nil || !strings.Contains(err.Error(), "pid 2") {
		t.Errorf("error = %v, want one naming pid 2", err)
	}
}

// A PID that neither the platform nor ps knows exited on the way and is not
// worth a line in the log.
func TestMemory_ErrorNamesOnlyWhatPsStoodInFor(t *testing.T) {
	f := stubMeasure(t, map[int]int64{1: 100}, nil, "2 3\n")

	got, err := Memory([]int{1, 2, 3})
	if want := (map[int]int64{1: 100, 2: 3 * 1024}); !reflect.DeepEqual(got, want) {
		t.Errorf("Memory() = %v, want %v", got, want)
	}
	if len(f.psCalls) != 1 {
		t.Errorf("ps calls = %v, want one", f.psCalls)
	}
	if err == nil || !strings.HasSuffix(err.Error(), "pid 2") {
		t.Errorf("error = %v, want one naming pid 2 and not pid 3", err)
	}

	stubMeasure(t, nil, nil, "")
	if _, err := Memory([]int{3}); err != nil {
		t.Errorf("a pid that no one knows: error = %v, want none", err)
	}
}

func TestMemory_PlatformErrorFallsBackForAll(t *testing.T) {
	boom := errors.New("footprint is not installed")
	f := stubMeasure(t, nil, boom, "1 1\n2 2\n")

	got, err := Memory([]int{1, 2})
	if want := (map[int]int64{1: 1024, 2: 2048}); !reflect.DeepEqual(got, want) {
		t.Errorf("Memory() = %v, want %v", got, want)
	}
	if len(f.psCalls) != 1 || !strings.Contains(strings.Join(f.psCalls[0], " "), "-p 1,2") {
		t.Errorf("ps calls = %v, want exactly one with -p 1,2", f.psCalls)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want one wrapping %v", err, boom)
	}
}

// After a timeout the resident size is the number known to be wrong, so ps
// is not asked and the PIDs are left out.
func TestMemory_TimeoutLeavesThePIDsOut(t *testing.T) {
	f := stubMeasure(t, map[int]int64{1: 100}, context.DeadlineExceeded, "1 1\n2 2\n")

	got, err := Memory([]int{2, 1})
	if len(got) != 0 {
		t.Errorf("Memory() = %v, want nothing", got)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the deadline", err)
	} else if !strings.HasSuffix(err.Error(), "memory of pid 1, 2 left out") {
		t.Errorf("error = %q, want one naming pids 1 and 2", err)
	}
	if len(f.psCalls) != 0 {
		t.Errorf("ps calls = %v, want none", f.psCalls)
	}
}

func TestMemory_NoPIDs(t *testing.T) {
	f := stubMeasure(t, map[int]int64{1: 100}, nil, "")

	if got, err := Memory(nil); got != nil || err != nil {
		t.Errorf("Memory(nil) = %v, %v, want nil, nil", got, err)
	}
	if got, err := ExactMemory(nil); got != nil || err != nil {
		t.Errorf("ExactMemory(nil) = %v, %v, want nil, nil", got, err)
	}
	if len(f.measured) != 0 || len(f.psCalls) != 0 {
		t.Errorf("measured %v, ps %v, want no calls", f.measured, f.psCalls)
	}
}

func TestExactMemory_NeverAsksPs(t *testing.T) {
	f := stubMeasure(t, map[int]int64{1: 100}, nil, "2 2\n")

	got, err := ExactMemory([]int{1, 2})
	if err != nil || !reflect.DeepEqual(got, map[int]int64{1: 100}) {
		t.Errorf("ExactMemory() = %v, %v, want only pid 1", got, err)
	}
	if len(f.psCalls) != 0 {
		t.Errorf("ps calls = %v, want none", f.psCalls)
	}
}

func TestTreeMemory_PsStandsInForTheLeaderOfATheCopyThePlatformCouldNotMeasure(t *testing.T) {
	f := stubMeasure(t, map[int]int64{1: 500}, nil, "2 3\n")

	got, err := TreeMemory([][]int{{1, 11, 12}, {2, 21}})
	if want := (map[int]int64{1: 500, 2: 3 * 1024}); !reflect.DeepEqual(got, want) {
		t.Errorf("TreeMemory() = %v, want %v", got, want)
	}
	if len(f.measured) != 1 || !reflect.DeepEqual(f.measured[0], [][]int{{1, 11, 12}, {2, 21}}) {
		t.Errorf("the platform was asked %v, want both trees in one call", f.measured)
	}
	if len(f.psCalls) != 1 || f.psCalls[0][len(f.psCalls[0])-1] != "2" {
		t.Errorf("ps calls = %v, want exactly one for the leader 2 alone", f.psCalls)
	}
	if err == nil || !strings.HasSuffix(err.Error(), "pid 2") {
		t.Errorf("error = %v, want one naming pid 2", err)
	}
}

func TestTreeMemory_TimeoutLeavesTheCopiesOutAndNothingIsEmpty(t *testing.T) {
	f := stubMeasure(t, nil, context.DeadlineExceeded, "1 1\n")
	got, err := TreeMemory([][]int{{1, 11}, {}, {2}})
	if len(got) != 0 || !errors.Is(err, context.DeadlineExceeded) || !strings.HasSuffix(err.Error(), "memory of pid 1, 2 left out") {
		t.Errorf("TreeMemory() = %v, %v, want nothing and the deadline naming the leaders 1 and 2", got, err)
	}
	if len(f.psCalls) != 0 {
		t.Errorf("ps calls = %v, want none", f.psCalls)
	}
	if got, err := ExactTreeMemory([][]int{{}, nil}); got != nil || err != nil {
		t.Errorf("ExactTreeMemory(only empty trees) = %v, %v, want nil, nil", got, err)
	}
}

func TestCopyTrees(t *testing.T) {
	old := readTable
	defer func() { readTable = old }()
	readTable = func() ([]procInfo, error) {
		return []procInfo{
			{pid: 100, ppid: 1, pgid: 100},
			{pid: 101, ppid: 1, pgid: 100},
			{pid: 102, ppid: 101, pgid: 102}, // detached, under a worker
			{pid: 200, ppid: 50, pgid: 50},   // a copy of a daemon before groups
			{pid: 201, ppid: 200, pgid: 50},  // its child, in the daemon's group
			{pid: 51, ppid: 50, pgid: 50},    // the daemon's other child: not the copy's
		}, nil
	}
	got, err := CopyTrees([]int{100, 200, 300})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][]int{100: {100, 101, 102}, 200: {200, 201}, 300: {300}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CopyTrees = %v, want %v", got, want)
	}

	readTable = func() ([]procInfo, error) { return nil, errors.New("no table") }
	if got, err := CopyTrees([]int{100}); err == nil || got != nil {
		t.Errorf("CopyTrees without a table = %v, %v, want an error", got, err)
	}
	if got, err := CopyTrees(nil); err != nil || got != nil {
		t.Errorf("CopyTrees(nil) = %v, %v, want nil, nil", got, err)
	}
}
