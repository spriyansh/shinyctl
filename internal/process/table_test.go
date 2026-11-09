package process

import (
	"os"
	"reflect"
	"syscall"
	"testing"
)

func pids(procs []procInfo) []int {
	var out []int
	for _, p := range procs {
		out = append(out, p.pid)
	}
	return out
}

func TestCopyProcs(t *testing.T) {
	table := []procInfo{
		{pid: 100, ppid: 1, pgid: 100},                 // the copy's leader
		{pid: 101, ppid: 1, pgid: 100},                 // a worker re-parented to init, in the group
		{pid: 102, ppid: 100, pgid: 100},               // a worker, in the group
		{pid: 103, ppid: 100, pgid: 103},               // a callr worker: a child of the leader in a group of its own
		{pid: 104, ppid: 103, pgid: 103},               // its child
		{pid: 105, ppid: 101, pgid: 105},               // a child of the re-parented worker, in a group of its own
		{pid: 106, ppid: 1, pgid: 100, zombie: true},   // a zombie of the group
		{pid: 107, ppid: 100, pgid: 107, zombie: true}, // a zombie child outside it
		{pid: 108, ppid: 107, pgid: 107},               // below a zombie: not reachable through it
		{pid: 200, ppid: 1, pgid: 200},                 // a stranger
		{pid: 201, ppid: 200, pgid: 200},               // the stranger's child
	}

	in, out := copyProcs(table, 100, true)
	if want := []int{100, 101, 102}; !reflect.DeepEqual(pids(in), want) {
		t.Errorf("group = %v, want %v (leader first, the zombie skipped)", pids(in), want)
	}
	if want := []int{103, 105, 104}; !reflect.DeepEqual(pids(out), want) {
		t.Errorf("outside = %v, want %v (descendants of any member, no stranger, no zombie)", pids(out), want)
	}

	// A record from before groups: the leader and what descends from it.
	in, out = copyProcs(table, 100, false)
	if want := []int{100}; !reflect.DeepEqual(pids(in), want) {
		t.Errorf("legacy group = %v, want %v", pids(in), want)
	}
	if want := []int{102, 103, 104}; !reflect.DeepEqual(pids(out), want) {
		t.Errorf("legacy descendants = %v, want %v (a worker re-parented to init is lost)", pids(out), want)
	}

	// A leader that is gone: its group still has members.
	in, out = copyProcs(table[1:], 100, true)
	if want := []int{101, 102}; !reflect.DeepEqual(pids(in), want) {
		t.Errorf("group of a gone leader = %v, want %v", pids(in), want)
	}
	// The callr worker was re-parented when its parent went, and is lost; the
	// child of a member is still found.
	if want := []int{105}; !reflect.DeepEqual(pids(out), want) {
		t.Errorf("outside of a gone leader = %v, want %v", pids(out), want)
	}

	// A leader that is a zombie is not listed.
	zl := []procInfo{{pid: 100, ppid: 1, pgid: 100, zombie: true}, {pid: 101, ppid: 1, pgid: 100}}
	in, _ = copyProcs(zl, 100, true)
	if want := []int{101}; !reflect.DeepEqual(pids(in), want) {
		t.Errorf("group with a zombie leader = %v, want %v", pids(in), want)
	}
}

// The real table lists this process, with its own group and a start time that
// does not change between two reads.
func TestReadTable_ListsThisProcess(t *testing.T) {
	read := func() procInfo {
		table, err := readTable()
		if err != nil {
			t.Skipf("no process table here: %v", err)
		}
		for _, p := range table {
			if p.pid == os.Getpid() {
				return p
			}
		}
		t.Fatalf("pid %d is not in the table of %d processes", os.Getpid(), len(table))
		return procInfo{}
	}
	a, b := read(), read()
	if a.pgid != syscall.Getpgrp() || a.ppid != os.Getppid() || a.zombie {
		t.Errorf("this process = %+v, want group %d and parent %d, not a zombie", a, syscall.Getpgrp(), os.Getppid())
	}
	if a.start == 0 || a.start != b.start {
		t.Errorf("start times %d and %d, want the same non-zero value", a.start, b.start)
	}
}
