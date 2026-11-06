package process

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// pidsOf lists every child the tree was started with.
func (tr *tree) pidsOf(kinds ...string) []int {
	var out []int
	for _, k := range kinds {
		out = append(out, tr.children[k]...)
	}
	return out
}

func TestStop_EndsTheGroup(t *testing.T) {
	tr := startTree(t, true, "", "plain", "plain")
	pids := append([]int{tr.pid()}, tr.pidsOf("plain")...)
	if !IsGroupLeader(tr.pid()) {
		t.Fatal("the leader does not lead a group")
	}

	signaled, left, err := stopCopy(tr.pid(), tr.ours(), 5*time.Second, killGrace, sysLook)
	if err != nil || !signaled || left != 0 {
		t.Fatalf("stopCopy = (%v, %d, %v), want (true, 0, nil)", signaled, left, err)
	}
	for _, pid := range pids {
		if !gone(pid) {
			t.Errorf("process %d is still there after Stop", pid)
		}
	}
}

// The leader leaves on SIGTERM and a worker shrugs it off: the group is then
// held by the worker alone, and only the member rule lets the SIGKILL round
// reach it.
func TestStop_LeaderLeavesFirst(t *testing.T) {
	tr := startTree(t, true, "", "plain", "ignore")
	worker := tr.children["ignore"][0]

	started := time.Now()
	signaled, left, err := stopCopy(tr.pid(), tr.ours(), 500*time.Millisecond, killGrace, sysLook)
	if err != nil || !signaled || left != 0 {
		t.Fatalf("stopCopy = (%v, %d, %v), want (true, 0, nil)", signaled, left, err)
	}
	if !gone(worker) {
		t.Errorf("the worker that ignores SIGTERM is still there")
	}
	if elapsed := time.Since(started); elapsed < 500*time.Millisecond {
		t.Errorf("Stop returned after %s, before the SIGTERM grace: the worker cannot have been killed by the SIGKILL round", elapsed)
	}
}

// A leader that is gone before Stop is asked nothing: its group id is not
// known to be the copy's any more.
func TestStop_LeaderGoneAtEntry(t *testing.T) {
	tr := startTree(t, true, "", "ignore")
	worker := tr.children["ignore"][0]

	syscall.Kill(tr.pid(), syscall.SIGKILL)
	if !gone(tr.pid()) {
		t.Fatal("the leader did not leave")
	}
	if signaled, left, err := stopCopy(tr.pid(), tr.ours(), 300*time.Millisecond, killGrace, sysLook); err != nil || signaled || left != 0 {
		t.Fatalf("stopCopy = (%v, %d, %v), want (false, 0, nil)", signaled, left, err)
	}
	if !running(t, worker) {
		t.Errorf("the worker of a copy that was already gone was signalled")
	}
}

func TestStop_DetachedChild(t *testing.T) {
	tr := startTree(t, true, "", "setsid", "setsid+ignore", "orphan")
	if sid, _ := unix.Getsid(tr.children["setsid"][0]); sid != tr.children["setsid"][0] {
		t.Fatalf("the detached child is not in a session of its own")
	}
	orphan := tr.children["orphan"][0]
	if ppid := ppidOf(t, orphan); ppid == tr.pid() {
		t.Fatalf("the orphan is still a child of the leader")
	}

	started := time.Now()
	signaled, left, err := stopCopy(tr.pid(), tr.ours(), 500*time.Millisecond, killGrace, sysLook)
	if err != nil || !signaled || left != 0 {
		t.Fatalf("stopCopy = (%v, %d, %v), want (true, 0, nil)", signaled, left, err)
	}
	for _, pid := range tr.pidsOf("setsid", "setsid+ignore") {
		if !gone(pid) {
			t.Errorf("detached child %d is still there", pid)
		}
	}
	// The one that ignores SIGTERM can only have been ended after the grace
	// period, when its group is long gone: by pid, under its start time.
	if elapsed := time.Since(started); elapsed < 500*time.Millisecond {
		t.Errorf("Stop returned after %s, before the SIGTERM grace", elapsed)
	}
	if !running(t, orphan) {
		t.Errorf("a process that is no longer a descendant of the copy was killed")
	}
}

func ppidOf(t *testing.T, pid int) int {
	t.Helper()
	table, err := readTable()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range table {
		if p.pid == pid {
			return p.ppid
		}
	}
	return -1
}

// A record from a daemon before groups: the leader is a child of the daemon
// and in its group. The stand-in daemon, in a group of its own, must survive.
func TestStop_OlderRecord(t *testing.T) {
	out := t.TempDir() + "/pids"
	daemon := helperCommand("daemon", helperChildren+"=plain,ignore", helperOut+"="+out)
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	go daemon.Wait()
	t.Cleanup(func() { syscall.Kill(-daemon.Process.Pid, syscall.SIGKILL) })

	tr := &tree{children: map[string][]int{}}
	tr.read(t, out)
	t.Cleanup(func() {
		for _, pids := range tr.children {
			for _, pid := range pids {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	// The leader is the daemon's child.
	table, err := readTable()
	if err != nil {
		t.Fatal(err)
	}
	leader := 0
	for _, p := range table {
		if p.ppid == daemon.Process.Pid {
			leader = p.pid
		}
	}
	if leader == 0 || IsGroupLeader(leader) {
		t.Fatalf("leader = %d, want a child of the daemon that does not lead a group", leader)
	}
	app := sleeperApp(leader, "sleep")

	started := time.Now()
	signaled, left, err := stopCopy(leader, func() bool { return isOurs(app) }, 500*time.Millisecond, killGrace, sysLook)
	if err != nil || !signaled || left != 0 {
		t.Fatalf("stopCopy = (%v, %d, %v), want (true, 0, nil)", signaled, left, err)
	}
	for _, pid := range append([]int{leader}, tr.pidsOf("plain", "ignore")...) {
		if !gone(pid) {
			t.Errorf("process %d is still there", pid)
		}
	}
	if elapsed := time.Since(started); elapsed < 500*time.Millisecond {
		t.Errorf("Stop returned after %s: the worker that ignores SIGTERM cannot have been ended by the SIGKILL round", elapsed)
	}
	if !running(t, daemon.Process.Pid) {
		t.Errorf("the daemon in whose group the leader ran was signalled")
	}
}

// A zombie in the group, whose parent is alive and does not reap it, answers
// kill(-P, 0) as if the group were there (EPERM on macOS, success on Linux).
// The table does not count it, so the stop does not wait for the grace.
func TestStop_ZombieInTheGroup(t *testing.T) {
	exe, _ := os.Executable()
	leader := exec.Command(exe, "sleep", "zombie-leader")
	leader.Env = append(os.Environ(), sleeperEnv+"=1")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	go leader.Wait()
	t.Cleanup(func() { syscall.Kill(-leader.Process.Pid, syscall.SIGKILL) })
	pgid := leader.Process.Pid

	// Y joins X's group and exits at once; its parent, this process, does not
	// wait for it until the cleanup.
	y := exec.Command("true")
	y.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := y.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { y.Wait() })
	deadline := time.Now().Add(3 * time.Second)
	for {
		table, err := readTable()
		if err != nil {
			t.Fatal(err)
		}
		zombie := false
		for _, p := range table {
			if p.pid == y.Process.Pid && p.zombie {
				zombie = true
			}
		}
		if zombie {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the short-lived process never became a zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}

	app := sleeperApp(pgid, "sleep")
	started := time.Now()
	signaled, left, err := stopCopy(pgid, func() bool { return isOurs(app) }, 5*time.Second, killGrace, sysLook)
	if err != nil || !signaled || left != 0 {
		t.Fatalf("stopCopy = (%v, %d, %v), want (true, 0, nil)", signaled, left, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("Stop took %s with only a zombie left in the group", elapsed)
	}
}

// The waiting step on its own: a member that stays live, and a probe that never
// says the group is gone (an ordinary user can't make a process that SIGKILL
// won't end), come back as a count at the deadline.
func TestWaitCopyGone_CountsWhatIsLeft(t *testing.T) {
	l := look{
		table: func() ([]procInfo, error) {
			return []procInfo{
				{pid: 10, ppid: 1, pgid: 10, start: 1},
				{pid: 11, ppid: 1, pgid: 10, start: 2},
				{pid: 12, ppid: 1, pgid: 10, start: 3, zombie: true},
			}, nil
		},
		sig0: func(pid int) error {
			if pid == 10 {
				return syscall.ESRCH // the leader is gone
			}
			return syscall.EPERM // the group never is
		},
	}
	leaderUp, left := waitCopyGone(l, 10, true, nil, 200*time.Millisecond)
	if leaderUp || left != 1 {
		t.Errorf("waitCopyGone = (%v, %d), want (false, 1): one live member, the zombie not counted", leaderUp, left)
	}

	// The table can't be read: nothing is known to be left, so the wait ends.
	l.table = func() ([]procInfo, error) { return nil, errors.New("no table") }
	started := time.Now()
	if leaderUp, left := waitCopyGone(l, 10, true, nil, 5*time.Second); leaderUp || left != 0 || time.Since(started) > time.Second {
		t.Errorf("without a table: (%v, %d) after %s, want (false, 0) at once", leaderUp, left, time.Since(started))
	}

}

// A process that nothing can end is counted, not turned into an error: the
// leader and the real workers go, and a member that the table keeps listing
// (and a group that never reads as gone) comes back as left.
func TestStop_WhatCannotBeEndedIsCountedNotAnError(t *testing.T) {
	tr := startTree(t, true, "", "plain")
	stuck := procInfo{pid: 999999, ppid: 1, pgid: tr.pid(), start: 7}
	l := look{
		table: func() ([]procInfo, error) {
			table, err := readTable()
			return append(table, stuck), err
		},
		sig0: func(pid int) error {
			if pid < 0 {
				return syscall.EPERM
			}
			return syscall.Kill(pid, 0)
		},
	}
	signaled, left, err := stopCopy(tr.pid(), tr.ours(), 300*time.Millisecond, 300*time.Millisecond, l)
	if err != nil || !signaled || left != 1 {
		t.Fatalf("stopCopy = (%v, %d, %v), want (true, 1, nil)", signaled, left, err)
	}
	if !gone(tr.pid()) || !gone(tr.children["plain"][0]) {
		t.Errorf("what could be ended was not")
	}
}

func TestKill_Group(t *testing.T) {
	tr := startTree(t, true, "ignore", "plain", "ignore")
	pids := append([]int{tr.pid()}, tr.pidsOf("plain", "ignore")...)

	hooked := 0
	signaled, others, err := Kill(sleeperApp(tr.pid(), "sleep"), func() { hooked++ })
	if err != nil || !signaled || others != 2 || hooked != 1 {
		t.Fatalf("Kill = (%v, %d, %v) with %d hook call(s), want (true, 2, nil) and 1", signaled, others, err, hooked)
	}
	for _, pid := range pids {
		if !gone(pid) {
			t.Errorf("process %d is still there after Kill", pid)
		}
	}
}

func TestKill_DetachedChild(t *testing.T) {
	tr := startTree(t, true, "", "setsid+ignore", "orphan")
	detached := tr.children["setsid+ignore"][0]
	orphan := tr.children["orphan"][0]

	signaled, others, err := Kill(sleeperApp(tr.pid(), "sleep"), nil)
	if err != nil || !signaled || others != 1 {
		t.Fatalf("Kill = (%v, %d, %v), want (true, 1, nil)", signaled, others, err)
	}
	if !gone(detached) {
		t.Errorf("the detached child is still there after Kill")
	}
	if !running(t, orphan) {
		t.Errorf("a process that is not a descendant of the copy was killed")
	}
}

func TestKill_OlderRecord(t *testing.T) {
	// A leader and a worker in the test's own group: the group must not be
	// signalled, and the leader's child must go.
	tr := startTree(t, false, "", "plain")
	signaled, others, err := Kill(sleeperApp(tr.pid(), "sleep"), nil)
	if err != nil || !signaled || others != 1 {
		t.Fatalf("Kill = (%v, %d, %v), want (true, 1, nil)", signaled, others, err)
	}
	if !gone(tr.pid()) || !gone(tr.children["plain"][0]) {
		t.Errorf("the leader or its child is still there after Kill")
	}
	if err := syscall.Kill(os.Getpid(), 0); err != nil {
		t.Errorf("the test process itself: %v", err)
	}
}

func TestKill_NoTable(t *testing.T) {
	tr := startTree(t, true, "", "plain")
	old := readTable
	defer func() { readTable = old }()
	readTable = func() ([]procInfo, error) { return nil, errors.New("no table") }

	signaled, others, err := Kill(sleeperApp(tr.pid(), "sleep"), nil)
	if err != nil || !signaled || others != 0 {
		t.Fatalf("Kill = (%v, %d, %v), want (true, 0, nil): the group is still signalled", signaled, others, err)
	}
	if !gone(tr.pid()) || !gone(tr.children["plain"][0]) {
		t.Errorf("the group was not killed without a table")
	}
}

// Stop and Kill mark the copy as being ended while they work.
func TestEnding(t *testing.T) {
	release := markEnding(4242)
	again := markEnding(4242)
	if !isEnding(4242) || isEnding(4243) {
		t.Fatal("markEnding did not mark exactly its pid")
	}
	release()
	release() // twice is once
	if !isEnding(4242) {
		t.Error("one of two ends released the mark")
	}
	again()
	if isEnding(4242) {
		t.Error("the mark stayed after every end released it")
	}

	tr := startTree(t, true, "", "plain")
	if _, _, err := Kill(sleeperApp(tr.pid(), "sleep"), nil); err != nil {
		t.Fatal(err)
	}
	if !isEnding(tr.pid()) {
		t.Error("a Kill does not mark the copy as ending until its reap has had its time")
	}
}
