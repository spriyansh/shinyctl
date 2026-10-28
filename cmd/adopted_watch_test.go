/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

// adoptedCount is how many of pids have an adopted watcher.
// skipWithoutWatching skips the test where the system refuses to watch a
// process that is not our child (a kernel before 5.3, a sandbox), which is
// the case the fallback to the tick is for.
func skipWithoutWatching(t *testing.T) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	_, err := process.WatchExit(manifest.AppSpec{}, state.AppState{PID: cmd.Process.Pid, StartedAt: time.Now(), EntrypointFile: "sleep 30"})
	if errors.Is(err, process.ErrWatchUnsupported) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EPERM) {
		t.Skipf("this system can't watch a process: %v", err)
	}
}

func adoptedCount(pids ...int) (n int) {
	for _, pid := range pids {
		if _, ok := adoptedWatched.Load(pid); ok {
			n++
		}
	}
	return n
}

// TestAdopted_ActiveCopyDiesAndTheWarmOneTakesOverAtOnce: copies left by an
// earlier run have no exit-watcher of the daemon's; the first look at them
// gives them one, so that when the active copy dies the warm copy is promoted
// then, not at the next tick (up to five seconds, which no test waits for:
// here nothing but the watcher reconciles). Its exit is counted once.
func TestAdopted_ActiveCopyDiesAndTheWarmOneTakesOverAtOnce(t *testing.T) {
	skipWithoutWatching(t)
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	one, two := adoptCopies(t, spec)

	// Looking without acting (refreshPools, status) starts no watcher.
	if n := adoptedCount(one.PID, two.PID); n != 0 {
		t.Errorf("%d adopted watchers before any tick, want none", n)
	}
	reconcileApp(spec.AppStateFile) // the first tick to look at them
	pool := waitRoles(t, spec, 1, 1)
	if pool.Active[0] != dialOf(one) || pool.Warm[0] != dialOf(two) {
		t.Fatalf("pool = %+v, want copy 1 active and copy 2 warm", pool)
	}
	if n := adoptedCount(one.PID, two.PID); n != 2 {
		t.Errorf("%d adopted watchers, want 2", n)
	}
	reconcileApp(spec.AppStateFile)
	reconcileApp(spec.AppStateFile)
	if n := adoptedCount(one.PID, two.PID); n != 2 {
		t.Errorf("%d adopted watchers after more ticks, want still 2", n)
	}

	killed := time.Now()
	if err := syscall.Kill(one.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for {
		pool, _ = gateway.Pool(spec.AppStateFile)
		if len(pool.Active) == 1 && pool.Active[0] == dialOf(two) {
			break
		}
		if time.Since(killed) > 2500*time.Millisecond { // well under the 5 s tick, with room for a loaded machine
			t.Fatalf("pool = %+v 2.5 seconds after the active copy died, want the warm copy active", pool)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Logf("the warm copy took over after %v", time.Since(killed))

	waitUntil(t, "the exit to be counted", 5*time.Second, func() bool {
		rec, _ := loadCopyRecord(spec, 1)
		return rec.CrashCount == 1
	})
	// Neither more ticks nor the late watcher count it again.
	reconcileApp(spec.AppStateFile)
	time.Sleep(300 * time.Millisecond)
	reconcileApp(spec.AppStateFile)
	if rec, _ := loadCopyRecord(spec, 1); rec.CrashCount != 1 || !rec.LastExitFailed {
		t.Errorf("copy 1 = %+v, want its exit counted once, as a failure", rec)
	}
	waitUntil(t, "the watcher to be dropped", 2*time.Second, func() bool {
		_, ok := adoptedWatched.Load(one.PID)
		return !ok
	})
}

// captureLog collects what the daemon logs while the test runs.
func captureLog(t *testing.T) *logBuffer {
	t.Helper()
	buf := &logBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

// A system that can't watch is logged once for the whole run, whatever the
// number of copies and ticks, and leaves the tick to do it.
func TestAdopted_NoWatchSupportFallsBackToTheTick(t *testing.T) {
	buf := captureLog(t)
	noWatchOnce = sync.Once{}
	var asked int
	useHooks(t, hooks{WatchExit: func(manifest.AppSpec, state.AppState) (<-chan struct{}, error) {
		asked++
		return nil, errors.New("not here")
	}})
	spec := copiesSpec(t, "app", 1, 0)
	watchAdopted(spec, state.AppState{PID: 424242, Copy: 1})
	watchAdopted(spec, state.AppState{PID: 424243, Copy: 2})
	watchAdopted(spec, state.AppState{PID: 424242, Copy: 1})
	if asked != 3 || adoptedCount(424242, 424243) != 0 {
		t.Errorf("asked %d times, %d watchers kept; want 3 and 0", asked, adoptedCount(424242, 424243))
	}
	if n := strings.Count(buf.String(), "can't watch"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "every 5s") {
		t.Errorf("log = %q, want it to say how often they are checked", buf.String())
	}
}

// A process that isn't ours is not watched, and says nothing.
func TestAdopted_NotOursIsNotWatched(t *testing.T) {
	buf := captureLog(t)
	noWatchOnce = sync.Once{}
	useHooks(t, hooks{WatchExit: func(manifest.AppSpec, state.AppState) (<-chan struct{}, error) {
		return nil, process.ErrNotOurs
	}})
	watchAdopted(copiesSpec(t, "app", 1, 0), state.AppState{PID: 424244, Copy: 1})
	if n := adoptedCount(424244); n != 0 || buf.Len() != 0 {
		t.Errorf("%d watchers kept, log %q; want none and no log", n, buf.String())
	}
}

// One watcher per process, and none for a process the daemon started.
func TestAdopted_OneWatcherPerProcessAndNoneForOurOwn(t *testing.T) {
	var asked atomic.Int32
	exit := make(chan struct{})
	useHooks(t, hooks{WatchExit: func(manifest.AppSpec, state.AppState) (<-chan struct{}, error) {
		asked.Add(1)
		return exit, nil
	}})
	spec := copiesSpec(t, "app", 1, 0)
	// Nothing is reconciled when this exits: the app isn't managed.
	watchAdopted(spec, state.AppState{PID: 424245, Copy: 1})
	watchAdopted(spec, state.AppState{PID: 424245, Copy: 1})
	if asked.Load() != 1 {
		t.Errorf("asked %d times for one process, want 1", asked.Load())
	}
	watched.Store(424246, struct{}{})
	defer watched.Delete(424246)
	watchAdopted(spec, state.AppState{PID: 424246, Copy: 1})
	if asked.Load() != 1 || adoptedCount(424246) != 0 {
		t.Errorf("asked %d times, want the daemon's own process left alone", asked.Load())
	}
	close(exit)
	waitUntil(t, "the watcher to be dropped", 2*time.Second, func() bool { return adoptedCount(424245) == 0 })
}

// Copies from an earlier run are watched as soon as the daemon is ready, not
// at its first tick: here no tick runs at all, one is killed at once, and the
// warm copy is active within a second; its exit is counted as a crash.
func TestAdopted_WatchedRightAtStartup(t *testing.T) {
	skipWithoutWatching(t)
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	one, two := adoptCopies(t, spec)
	if n := adoptedCount(one.PID, two.PID); n != 0 {
		t.Fatalf("%d adopted watchers before the daemon was ready, want none", n)
	}

	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	exe, _ := os.Executable()
	_, sup, err := newGateway(dir, exe, filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	sup.OnReady() // what the daemon does when the load balancer is up
	if n := adoptedCount(one.PID, two.PID); n != 2 {
		t.Fatalf("%d adopted watchers after OnReady, want 2", n)
	}
	pool := waitRoles(t, spec, 1, 1)
	if pool.Active[0] != dialOf(one) {
		t.Fatalf("pool = %+v, want copy 1 active", pool)
	}

	killed := time.Now()
	if err := syscall.Kill(one.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for {
		pool, _ = gateway.Pool(spec.AppStateFile)
		if len(pool.Active) == 1 && pool.Active[0] == dialOf(two) {
			break
		}
		if time.Since(killed) > 4*time.Second { // under the 5 s tick, which does not run here, so the deadline tells the watcher from none; room for a loaded machine
			t.Fatalf("pool = %+v 4 seconds after the kill, want the warm copy active", pool)
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitUntil(t, "the exit to be counted", 5*time.Second, func() bool {
		rec, _ := loadCopyRecord(spec, 1)
		return rec.CrashCount == 1 && rec.LastExitFailed
	})
}

// The last copy of an app that does not restart dies: the app gives up, and
// the port it was served on closes at once, which takes the sync after the
// reconcile (the tick would do it up to five seconds later).
func TestAdopted_GivingUpClosesThePortAtOnce(t *testing.T) {
	skipWithoutWatching(t)
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	spec.RestartPolicy = manifest.RestartNever
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
	})
	a = startCopy(t, spec)
	waitServed(t, spec, a.PID)
	reconcileApp(spec.AppStateFile) // sees it, and starts watching it
	if _, ok := adoptedWatched.Load(a.PID); !ok {
		t.Fatal("no watcher on the adopted copy")
	}

	killed := time.Now()
	if err := syscall.Kill(a.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for process.IsReady(spec.Host, spec.Port) {
		if time.Since(killed) > 2500*time.Millisecond { // well under the 5 s tick, with room for a loaded machine
			t.Fatalf("%s:%d still answers 2.5 seconds after the last copy died and the app gave up", spec.Host, spec.Port)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// After a reboot every copy is gone and the new daemon never saw them alive.
// The load balancer comes up (OnReady) before the first tick, and its look at
// the copies must leave the dead ones unseen: they just start, with nothing
// counted, even under a restart policy that gives up on a crash.
func TestAdopted_AfterARebootOnReadyCountsNothing(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	spec.RestartPolicy = manifest.RestartNever
	cleanupApp(t, spec)
	one, two := adoptCopies(t, spec)
	for _, rec := range []state.AppState{one, two} {
		syscall.Kill(rec.PID, syscall.SIGKILL)
		waitGone(t, rec)
		forgetSeen(spec.AppStateFile, rec.Copy)
	}
	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	exe, _ := os.Executable()
	_, sup, err := newGateway(dir, exe, filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	sup.OnReady()
	if !reconcileApp(spec.AppStateFile) {
		t.Fatal("the tick did not start the copies")
	}
	for _, old := range []state.AppState{one, two} {
		got, _ := loadCopyRecord(spec, old.Copy)
		if got.PID == old.PID || got.PID == 0 || got.CrashCount != 0 || !got.LastExitAt.IsZero() {
			t.Errorf("copy %d = %+v, want a new process and no crash counted", old.Copy, got)
		}
	}
}
