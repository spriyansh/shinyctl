package process

import (
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/spriyansh/shinyctl/internal/state"
)

// stopGrace is how long Stop gives a process to exit on SIGTERM before
// escalating to SIGKILL. R honors SIGTERM, but not mid-computation — a
// Shiny session deep in a long calculation can sit on it for a while.
const stopGrace = 10 * time.Second

// killGrace is how long Stop waits after SIGKILL for the process to
// actually be gone — the kill itself is immediate; this covers the reap.
const killGrace = 2 * time.Second

// StopWait is the longest StopIf takes to end a process: the SIGTERM grace
// period, then the wait after SIGKILL.
const StopWait = stopGrace + killGrace

// Stop ends app's recorded copy, the process and everything it started, and
// only returns once the leader is gone: SIGTERM first, then SIGKILL for
// whatever is left after stopGrace. The caller's next step is clearing the
// record from state.json, and a record cleared while the process is still
// running (and still holding its port) turns the next apply's port check into
// a phantom crash. A PID that's already gone is treated as success: "make sure
// it's stopped" trivially holds if it already is.
//
// The copy is its process group (every copy leads one, see Start) and the
// processes that left the group but descend from it, such as a callr worker,
// which has a session of its own. A record from a daemon that did not start
// copies in groups has no group; its processes are the leader and its
// descendants. The processes are read once, while the leader is alive and so
// pins the group id; the SIGKILL round signals the group only while the leader
// is still ours or a process noted at that first read is still in the group,
// and the others only if a fresh read shows the same pid with the same start
// time, because a grace period is long enough for a process that exited to
// have its pid handed to a stranger. Only the group is ever signalled as a
// group, never "the group of this pid".
//
// A process of the copy that is still there when the leader is gone and the
// SIGKILL round has had killGrace is not an error: a worker in uninterruptible
// sleep on a stalled mount cannot be ended by anyone, and must not turn a stop
// into a failure or keep a record. It is counted in left for the caller to log.
//
// It signals only while the process is identifiably the app's own (see
// isOurs; fallbacks are for records that predate the recorded entrypoint).
// signaled=false means there was nothing of ours to stop (no PID, already
// gone, or someone else's); that counts as success.
func Stop(app state.AppState, fallbacks ...string) (signaled bool, left int, err error) {
	return stopCopy(app.PID, func() bool { return isOurs(app, fallbacks...) }, stopGrace, killGrace, sysLook)
}

// Kill ends app's recorded copy at once with SIGKILL, with no grace period and
// without waiting for it to be gone: the caller wants it dead now and watches
// for its exit itself. It signals under Stop's rule, only while the process is
// identifiably the app's own, so a PID that was handed to a stranger is left
// alone. The copy's group and the processes that descend from it are killed
// with it.
//
// beforeSignal, when it is not nil, is called once the process has been
// recognised and just before the signal, so that a caller can note something
// about a process that is certainly the app's. It is not called when nothing
// of ours is there. signaled=false means there was nothing to kill (no PID,
// already gone, or someone else's), which is not an error. others is how many
// processes besides the leader the copy had when it was read.
func Kill(app state.AppState, beforeSignal func(), fallbacks ...string) (signaled bool, others int, err error) {
	pid := app.PID
	if pid <= 0 || !isOurs(app, fallbacks...) {
		return false, 0, nil
	}

	// The leader is alive and ours, so it pins the group: whoever is in it is
	// the copy's. The table is read before the signal, in-process and in about
	// a millisecond; if it can't be read, the group (or the leader alone)
	// still goes.
	group := IsGroupLeader(pid)
	var inGroup, outside []procInfo
	if table, err := readTable(); err == nil {
		inGroup, outside = copyProcs(table, pid, group)
	}

	// The copy counts as being ended until the reap has had its time, so that
	// the exit this causes is not mistaken for one the copy chose.
	release := markEnding(pid)
	time.AfterFunc(killGrace, release)

	if beforeSignal != nil {
		beforeSignal()
	}
	target := pid
	if group {
		target = -pid
	}
	if err := syscall.Kill(target, syscall.SIGKILL); err != nil {
		release()
		if err == syscall.ESRCH {
			return false, 0, nil
		}
		return false, 0, err
	}
	for _, p := range outside {
		syscall.Kill(p.pid, syscall.SIGKILL) // gone already is fine
	}

	for _, p := range inGroup {
		if p.pid != pid {
			others++
		}
	}
	return true, others + len(outside), nil
}

// ending holds the pids of the copies that Stop or Kill is ending, with how
// many are, so that Sweep does not treat the exit they cause as one the copy
// chose.
var ending struct {
	sync.Mutex
	n map[int]int
}

// markEnding notes that pid's copy is being ended, and returns the function
// that takes the note back. The function is safe to call more than once.
func markEnding(pid int) (release func()) {
	ending.Lock()
	if ending.n == nil {
		ending.n = map[int]int{}
	}
	ending.n[pid]++
	ending.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			ending.Lock()
			defer ending.Unlock()
			if ending.n[pid]--; ending.n[pid] <= 0 {
				delete(ending.n, pid)
			}
		})
	}
}

func isEnding(pid int) bool {
	ending.Lock()
	defer ending.Unlock()
	return ending.n[pid] > 0
}

// IsGroupLeader reports whether pid leads a process group, which is how a copy
// started by this version looks: its pid is its group's id. A copy from an
// older daemon is in the daemon's group instead. A zombie is not asked
// about: macOS answers ESRCH for it and Linux answers, and either way a zombie
// leader is gone for the callers.
func IsGroupLeader(pid int) bool {
	pgid, err := syscall.Getpgid(pid)
	return err == nil && pgid == pid
}

// member is a process as it was noted at a read of the table. The start time
// is part of it because a pid alone could be reused.
type member struct {
	pid   int
	start uint64
}

func membersOf(procs []procInfo) []member {
	out := make([]member, len(procs))
	for i, p := range procs {
		out[i] = member{p.pid, p.start}
	}
	return out
}

// live reports whether table has m alive: the same pid with the same start
// time, not a zombie.
func live(table []procInfo, m member) bool {
	for _, p := range table {
		if p.pid == m.pid {
			return p.start == m.start && !p.zombie
		}
	}
	return false
}

// look is how a stop sees processes: the table, and kill(pid, 0). The tests
// replace them to stand in for processes that cannot be made to behave.
type look struct {
	table func() ([]procInfo, error)
	sig0  func(pid int) error
}

var sysLook = look{
	table: func() ([]procInfo, error) { return readTable() },
	sig0:  func(pid int) error { return syscall.Kill(pid, 0) },
}

// tableEvery is how often the waiting step reads the table. The leader is
// polled every 50 ms; the group and the other processes need the table, which
// costs more.
const tableEvery = 250 * time.Millisecond

// waitCopyGone waits up to timeout for the leader to be gone and for the rest
// of the copy to be: the group (kill(-leader, 0) says ESRCH, or the table has
// no live process in it; a group that holds only zombies answers EPERM on
// macOS and success on Linux, so the probe alone cannot end the wait on
// anything but ESRCH, and cannot be trusted to say "present") and every noted
// outside process (the table has no live process with its pid and start time).
// A probe of the group never ends the wait by itself. It returns at the
// deadline with whether the leader is still there and how many processes of the
// copy besides it are.
func waitCopyGone(l look, leader int, group bool, outside []member, timeout time.Duration) (leaderUp bool, left int) {
	deadline := time.Now().Add(timeout)
	var readAt time.Time
	var groupLeft, outsideLeft int
	for {
		leaderUp = l.sig0(leader) != syscall.ESRCH
		groupMaybe := group && l.sig0(-leader) != syscall.ESRCH

		// While the leader is up the wait cannot end, so the table is read
		// only for the count at the deadline.
		pastDeadline := time.Now().After(deadline)
		if (groupMaybe || len(outside) > 0) && (!leaderUp || pastDeadline) {
			if readAt.IsZero() || time.Since(readAt) >= tableEvery || pastDeadline {
				readAt = time.Now()
				groupLeft, outsideLeft = 0, 0
				// An unreadable table can't say what is left, and a wait that
				// ran to its deadline on that would turn a stop into a delay.
				if table, err := l.table(); err == nil {
					for _, p := range table {
						if p.pgid == leader && p.pid != leader && !p.zombie {
							groupLeft++
						}
					}
					for _, m := range outside {
						if live(table, m) {
							outsideLeft++
						}
					}
				}
			}
		} else {
			groupLeft, outsideLeft = 0, 0
		}
		left = outsideLeft
		if groupMaybe {
			left += groupLeft
		}

		if !leaderUp && left == 0 {
			return false, 0
		}
		if pastDeadline {
			return leaderUp, left
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stopCopy is Stop with the identity check, the grace periods and the look at
// processes as parameters, for tests.
func stopCopy(pid int, ours func() bool, grace, kgrace time.Duration, l look) (bool, int, error) {
	if pid <= 0 || !ours() {
		return false, 0, nil
	}
	defer markEnding(pid)()

	// The first read, while the live leader pins the group id.
	group := IsGroupLeader(pid)
	var inGroup, outside []procInfo
	if table, err := l.table(); err == nil {
		inGroup, outside = copyProcs(table, pid, group)
	}
	noted, others := membersOf(inGroup), membersOf(outside)

	target := pid
	if group {
		target = -pid
	}
	if err := syscall.Kill(target, syscall.SIGTERM); err != nil {
		if err != syscall.ESRCH {
			return true, 0, err
		}
		if len(others) == 0 {
			return true, 0, nil
		}
	}
	for _, m := range others {
		syscall.Kill(m.pid, syscall.SIGTERM)
	}

	leaderUp, left := waitCopyGone(l, pid, group, others, grace)
	if !leaderUp && left == 0 {
		return true, 0, nil
	}

	// The SIGKILL round, for whatever of the copy is left. The leader is
	// asked about again: the grace period is long enough for it to have gone
	// and its pid to have been handed to someone else.
	leaderOurs := leaderUp && ours()
	table, terr := l.table()
	killGroup := false
	if group {
		killGroup = leaderOurs
		if !killGroup && terr == nil {
			// The leader is gone: the group is signalled only while a process
			// noted at the first read is still in it, which also proves that
			// group id is still the copy's (a live member keeps it from being
			// handed out).
			for _, m := range noted {
				if live(table, m) && groupOf(table, m.pid) == pid {
					killGroup = true
					break
				}
			}
		}
	}
	switch {
	case killGroup:
		syscall.Kill(-pid, syscall.SIGKILL)
	case !group && leaderOurs:
		syscall.Kill(pid, syscall.SIGKILL)
	}
	if terr == nil {
		for _, m := range others {
			if live(table, m) {
				syscall.Kill(m.pid, syscall.SIGKILL)
			}
		}
	}

	leaderUp, left = waitCopyGone(l, pid, group, others, kgrace)
	if leaderUp && ours() {
		return true, left, fmt.Errorf("process %d still running %s after SIGKILL", pid, kgrace)
	}
	return true, left, nil
}

// groupOf is the process group of pid in table, or 0.
func groupOf(table []procInfo, pid int) int {
	for _, p := range table {
		if p.pid == pid {
			return p.pgid
		}
	}
	return 0
}

// StopIf is Stop for a process that isn't an app's, such as the load
// balancer: SIGTERM, SIGKILL after the same grace period, each signal only
// while ours reports that pid is still the process the caller means.
func StopIf(pid int, ours func() bool) (signaled bool, err error) {
	return stop(pid, ours, stopGrace)
}

// stop is Stop with the identity check and the SIGTERM grace period as
// parameters, for tests.
func stop(pid int, ours func() bool, grace time.Duration) (bool, error) {
	if pid <= 0 || !ours() {
		return false, nil
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return true, nil
		}
		return true, err
	}
	if waitGone(pid, grace) {
		return true, nil
	}
	if !ours() {
		return true, nil
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return true, err
	}
	if waitGone(pid, killGrace) || !ours() {
		return true, nil
	}

	return true, fmt.Errorf("process %d still running %s after SIGKILL", pid, killGrace)
}

// waitGone polls until pid no longer exists or timeout elapses. A zombie
// still "exists" to Kill(pid, 0); the exit-watcher goroutine's Wait()
// reaps our own children promptly, and init reaps anything we didn't
// start ourselves, so this resolves either way.
func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
