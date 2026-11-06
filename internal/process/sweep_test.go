package process

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func TestNoteGroup(t *testing.T) {
	table := []procInfo{
		{pid: 10, ppid: 1, pgid: 10, start: 1},
		{pid: 11, ppid: 1, pgid: 10, start: 2},
		{pid: 12, ppid: 1, pgid: 10, start: 3, zombie: true},
		{pid: 20, ppid: 1, pgid: 20, start: 4},
	}
	l := look{table: func() ([]procInfo, error) { return table, nil }}

	n, ok := noteGroup(10, l)
	if !ok || len(n.members) != 2 || n.members[0] != (member{10, 1}) || n.members[1] != (member{11, 2}) {
		t.Errorf("noteGroup(10) = %+v, %v, want the two live members with their start times", n, ok)
	}
	if _, ok := noteGroup(30, l); ok {
		t.Error("a group with no member gave a note")
	}
	if _, ok := noteGroup(0, l); ok {
		t.Error("group 0 gave a note")
	}
	l.table = func() ([]procInfo, error) { return nil, errors.New("no table") }
	if _, ok := noteGroup(10, l); ok {
		t.Error("an unreadable table gave a note")
	}

	release := markEnding(10)
	defer release()
	l.table = func() ([]procInfo, error) { return table, nil }
	if n, _ := noteGroup(10, l); !n.ending {
		t.Error("the note does not say the copy was being ended")
	}
}

// noteOf is a note of the given pids of a tree, as NoteGroup would take it.
func noteOf(t *testing.T, pgid int, pids ...int) Note {
	t.Helper()
	table, err := readTable()
	if err != nil {
		t.Fatal(err)
	}
	n := Note{pgid: pgid}
	for _, p := range table {
		for _, pid := range pids {
			if p.pid == pid {
				n.members = append(n.members, member{p.pid, p.start})
			}
		}
	}
	if len(n.members) != len(pids) {
		t.Fatalf("note of %v found %v", pids, n.members)
	}
	return n
}

// The leader crashed: the workers it left in the group are ended, the one that
// ignores SIGTERM by SIGKILL after the grace.
func TestSweep(t *testing.T) {
	tr := startTree(t, true, "", "plain", "ignore")
	plain, ignoring := tr.children["plain"][0], tr.children["ignore"][0]
	syscall.Kill(tr.pid(), syscall.SIGKILL)
	if !gone(tr.pid()) {
		t.Fatal("the leader did not leave")
	}
	n, ok := NoteGroup(tr.pid())
	if !ok || len(n.members) != 2 {
		t.Fatalf("NoteGroup = %+v, %v, want the two workers", n, ok)
	}

	started := time.Now()
	swept, left := sweep(n, 400*time.Millisecond, 2*time.Second, sysLook)
	if swept != 2 || left != 0 {
		t.Errorf("sweep = (%d, %d), want (2, 0)", swept, left)
	}
	if !gone(plain) || !gone(ignoring) {
		t.Errorf("a worker is still there after the sweep")
	}
	if time.Since(started) < 400*time.Millisecond {
		t.Errorf("the sweep took %s: the worker that ignores SIGTERM cannot have been killed after the grace", time.Since(started))
	}
}

// Noted members that have all left before the first signal: nothing is
// signalled, though the group has another, unnoted member (which could be a
// stranger that took the group id).
func TestSweep_NothingNotedIsLeftNoSignal(t *testing.T) {
	tr := startTree(t, true, "", "plain", "ignore")
	plain, other := tr.children["plain"][0], tr.children["ignore"][0]
	n := noteOf(t, tr.pid(), plain)
	syscall.Kill(plain, syscall.SIGKILL)
	if !gone(plain) {
		t.Fatal("the noted member did not leave")
	}

	if swept, left := sweep(n, 200*time.Millisecond, 200*time.Millisecond, sysLook); swept != 0 || left != 0 {
		t.Errorf("sweep = (%d, %d), want (0, 0)", swept, left)
	}
	if !running(t, other) || !running(t, tr.pid()) {
		t.Errorf("the group was signalled although no noted member was in it")
	}
}

// The member rule before the SIGKILL: the noted worker leaves on SIGTERM; the
// group's other member, never noted, ignores it and must not be killed.
func TestSweep_NoSIGKILLOnceTheNotedAreGone(t *testing.T) {
	tr := startTree(t, true, "ignore", "plain", "ignore")
	plain, other := tr.children["plain"][0], tr.children["ignore"][0]
	n := noteOf(t, tr.pid(), plain)

	if swept, left := sweep(n, 300*time.Millisecond, 300*time.Millisecond, sysLook); swept != 1 || left != 0 {
		t.Errorf("sweep = (%d, %d), want (1, 0)", swept, left)
	}
	if !gone(plain) {
		t.Error("the noted worker is still there")
	}
	time.Sleep(500 * time.Millisecond) // past a SIGKILL that must not come
	if !running(t, other) || !running(t, tr.pid()) {
		t.Errorf("the group got a SIGKILL although no noted member was left")
	}
}

func TestSweep_AnEndingCopyIsSkipped(t *testing.T) {
	tr := startTree(t, true, "", "plain")
	n := noteOf(t, tr.pid(), tr.children["plain"][0])

	release := markEnding(tr.pid())
	if swept, left := sweep(n, 200*time.Millisecond, 200*time.Millisecond, sysLook); swept != 0 || left != 0 {
		t.Errorf("sweep of a copy being stopped = (%d, %d), want (0, 0)", swept, left)
	}
	release()
	if !running(t, tr.children["plain"][0]) {
		t.Error("a copy that Stop was ending was swept")
	}

	// A note taken while Stop was at work says so for good.
	n.ending = true
	if swept, _ := sweep(n, 200*time.Millisecond, 200*time.Millisecond, sysLook); swept != 0 {
		t.Errorf("sweep of a note taken during a stop signalled %d", swept)
	}
}
