package process

import (
	"syscall"
	"time"
)

// Note is what NoteGroup found: the live processes of a copy's group at the
// moment its leader was seen to exit, each with its start time. It is the proof
// Sweep needs that the group it signals is still the copy's.
type Note struct {
	pgid    int
	members []member

	// ending says Stop or Kill was ending the copy when the note was taken, so
	// that the exit was theirs and not the copy's own.
	ending bool
}

// NoteGroup reads the live members of the process group pgid, the group of a
// copy whose leader has just exited. It is called the moment an exit is seen,
// before anything that can wait (the state lock a reconcile cycle may hold for
// seconds), because the group is only known to be the copy's while a member
// pins its id and the first read is the one that names the members. It is one
// read of the process table, and false when the group has no live process or
// the table can't be read.
func NoteGroup(pgid int) (Note, bool) {
	return noteGroup(pgid, sysLook)
}

func noteGroup(pgid int, l look) (Note, bool) {
	if pgid <= 0 {
		return Note{}, false
	}
	table, err := l.table()
	if err != nil {
		return Note{}, false
	}
	n := Note{pgid: pgid, ending: isEnding(pgid)}
	for _, p := range table {
		if p.pgid == pgid && !p.zombie {
			n.members = append(n.members, member{p.pid, p.start})
		}
	}
	return n, len(n.members) > 0
}

// Sweep ends what a copy that exited on its own left in its group (a crash, an
// R error, the kernel's OOM killer), the way Stop would have: SIGTERM, and
// SIGKILL after stopGrace for what is still there. It returns how many
// processes it signalled and how many were still there at the end.
//
// Every group signal is checked against the note taken at the exit: it goes
// only while at least one noted process (same pid and start time) is still in
// the group, which proves the group id has not been handed to a stranger. A
// note whose members have all left before the first signal gets no signal at
// all. A copy that Stop or Kill was ending is left to them.
func Sweep(n Note) (swept, left int) {
	return sweep(n, stopGrace, killGrace, sysLook)
}

func sweep(n Note, grace, kgrace time.Duration, l look) (swept, left int) {
	if n.pgid <= 0 || len(n.members) == 0 || n.ending || isEnding(n.pgid) {
		return 0, 0
	}

	held := func() (int, bool) {
		table, err := l.table()
		if err != nil {
			return 0, false
		}
		count := 0
		for _, m := range n.members {
			if live(table, m) && groupOf(table, m.pid) == n.pgid {
				count++
			}
		}
		return count, true
	}

	count, ok := held()
	if !ok || count == 0 {
		return 0, 0
	}
	syscall.Kill(-n.pgid, syscall.SIGTERM)
	swept = count

	count = waitHeld(held, grace)
	if count > 0 {
		// Fresh: the member rule is checked again before the SIGKILL.
		if count, ok = held(); ok && count > 0 {
			syscall.Kill(-n.pgid, syscall.SIGKILL)
			count = waitHeld(held, kgrace)
		}
	}
	return swept, count
}

// waitHeld polls until none of the noted members is left or timeout passes,
// and returns how many are. A table that can't be read ends the wait.
func waitHeld(held func() (int, bool), timeout time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		count, ok := held()
		if !ok || count == 0 {
			return 0
		}
		if time.Now().After(deadline) {
			return count
		}
		time.Sleep(50 * time.Millisecond)
	}
}
