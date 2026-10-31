/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

const (
	mib     = 1 << 20
	limitMB = 64 // the limit of limitedSpec, the least Validate accepts
)

// limitedSpec is copiesSpec with a max-memory of limitMB.
func limitedSpec(t *testing.T, name string, replicas, warm int) manifest.AppSpec {
	t.Helper()
	spec := copiesSpec(t, name, replicas, warm)
	spec.MaxMemory = fmt.Sprintf("%dM", limitMB)
	return spec
}

// memoryStub is a measure that answers what the test says and remembers what
// it was asked.
type memoryStub struct {
	mu    sync.Mutex
	sizes map[int]int64
	err   error
	asked [][]int
}

func (s *memoryStub) measure(pids []int) (map[int]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, slices.Clone(pids))
	if s.err != nil {
		return nil, s.err
	}
	out := map[int]int64{}
	for _, pid := range pids {
		if size, ok := s.sizes[pid]; ok {
			out[pid] = size
		}
	}
	return out, nil
}

func (s *memoryStub) set(sizes map[int]int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sizes, s.err = sizes, err
}

func (s *memoryStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.asked)
}

// resetMemoryState forgets what the loop remembers, at the start and the end
// of a test, so that what one test left (a spell, a kill) is not another's.
func resetMemoryState(t *testing.T) {
	t.Helper()
	reset := func() {
		killed.Range(func(k, _ any) bool { killed.Delete(k); return true })
		memorySpells.Lock()
		memorySpells.failed, memorySpells.unmeasured, memorySpells.unparsable = false, nil, nil
		memorySpells.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func runOnce(t *testing.T, stub *memoryStub) []unwatchedKill {
	t.Helper()
	return enforceMemoryOnce(context.Background(), stub.measure)
}

// readCopyLog returns the log of copy n of spec.
func readCopyLog(t *testing.T, spec manifest.AppSpec, n int) string {
	t.Helper()
	path, err := process.CopyLogPath(spec, n)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	return string(data)
}

func TestMemory_OverTheLimitIsKilledAndTheWarmCopyTakesOver(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	logs := captureLog(t)
	spec := limitedSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active, warm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])
	_, cookie := visit(t, spec, nil)

	stub := &memoryStub{sizes: map[int]int64{active.PID: 100 * mib, warm.PID: 10 * mib}}
	if pending := runOnce(t, stub); len(pending) != 0 {
		t.Errorf("the run asks for a reconcile of %+v, but the copy is watched", pending)
	}
	if len(stub.asked) != 1 || len(stub.asked[0]) != 2 {
		t.Errorf("the measure was asked %v, want one call for both copies", stub.asked)
	}

	// Before any reconcile: the loop itself gave the visitors to the warm copy.
	if p, _ := gateway.Pool(spec.AppStateFile); !slices.Equal(p.Active, []string{dialOf(warm)}) || len(p.Warm) != 0 {
		t.Errorf("pool = %+v right after the kill, want only the warm copy, active", p)
	}
	if pid, _ := visit(t, spec, cookie); pid != warm.PID {
		t.Errorf("the visitor of the killed copy reached pid %d, want the warm copy %d", pid, warm.PID)
	}

	waitUntil(t, "the exit to be recorded", 5*time.Second, func() bool {
		rec, _ := loadCopyRecord(spec, active.Copy)
		return rec.CrashCount == 1
	})
	rec, _ := loadCopyRecord(spec, active.Copy)
	if !rec.LastExitFailed || rec.LastExitReason != "oom" || rec.PID != active.PID {
		t.Errorf("killed copy = %+v, want one failed exit with the reason oom", rec)
	}
	if other, _ := loadCopyRecord(spec, warm.Copy); other.CrashCount != 0 || other.PID != warm.PID || other.LastExitReason != "" || !process.IsAlive(spec, other) {
		t.Errorf("the other copy = %+v, want it untouched and running", other)
	}
	if marker := fmt.Sprintf("--- shinyctl: killed copy %d at ", active.Copy); !strings.Contains(readCopyLog(t, spec, active.Copy), marker) {
		t.Errorf("the copy's log has no %q line:\n%s", marker, readCopyLog(t, spec, active.Copy))
	}
	want := fmt.Sprintf(`memory: "app": copy %d (pid %d) holds 100M, over max-memory 64M; killed it; copy %d serves visitors`, active.Copy, active.PID, warm.Copy)
	if !strings.Contains(logs.String(), want) {
		t.Errorf("log = %q, want it to contain %q", logs.String(), want)
	}
}

func TestMemory_UnderTheLimitIsLeftAlone(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	a := applyOK(t, spec)

	stub := &memoryStub{sizes: map[int]int64{a.PID: limitMB * mib}} // at the limit is not over it
	runOnce(t, stub)
	if !process.IsAlive(spec, a) {
		t.Error("a copy at its limit was killed")
	}
	assertNoExitRecorded(t, spec, a.PID)
}

func TestMemory_AFailingMeasureKillsNothingAndIsLoggedOncePerSpell(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	logs := captureLog(t)
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	a := applyOK(t, spec)

	const said = "memory: can't measure the copies: footprint timed out; no limit is enforced until they can be"
	stub := &memoryStub{err: errors.New("footprint timed out")}
	for i := 0; i < 3; i++ {
		runOnce(t, stub)
	}
	if got := strings.Count(logs.String(), said); got != 1 {
		t.Errorf("the log has the line %d times after three failing runs, want 1:\n%s", got, logs.String())
	}
	if !process.IsAlive(spec, a) {
		t.Error("a copy was killed although nothing was measured")
	}

	// A run that measures ends the spell; the next failure is a new one.
	stub.set(map[int]int64{a.PID: 10 * mib}, nil)
	runOnce(t, stub)
	stub.set(nil, errors.New("footprint timed out"))
	runOnce(t, stub)
	if got := strings.Count(logs.String(), said); got != 2 {
		t.Errorf("the log has the line %d times after a second spell, want 2", got)
	}
}

func TestMemory_NothingToMeasureCostsNothing(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	logs := captureLog(t)
	stub := &memoryStub{err: errors.New("must not be asked")}

	// No app at all, an app with no limit, and an app with a limit whose
	// process is gone.
	runOnce(t, stub)
	free := copiesSpec(t, "free", 1, 0)
	cleanupApp(t, free)
	applyOK(t, free)
	limited := limitedSpec(t, "limited", 1, 0)
	cleanupApp(t, limited)
	deadRecord(t, limited)
	runOnce(t, stub)

	if stub.calls() != 0 {
		t.Errorf("the measure was called %d time(s), want none", stub.calls())
	}
	if strings.Contains(logs.String(), "memory:") {
		t.Errorf("the log says something about memory:\n%s", logs.String())
	}
}

func TestMemory_UnmeasurableCopyIsLoggedOnceAndNotKilled(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	logs := captureLog(t)
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	a := applyOK(t, spec)

	said := fmt.Sprintf(`memory: can't measure copy 1 (pid %d) of "app"; its limit is not enforced until it can be`, a.PID)
	stub := &memoryStub{sizes: map[int]int64{}}
	runOnce(t, stub)
	runOnce(t, stub)
	if got := strings.Count(logs.String(), said); got != 1 {
		t.Errorf("the log has the line %d times after two runs, want 1:\n%s", got, logs.String())
	}
	if !process.IsAlive(spec, a) {
		t.Error("a copy that could not be measured was killed")
	}

	// Measured, it is forgotten; unmeasurable again is a new spell.
	stub.set(map[int]int64{a.PID: 10 * mib}, nil)
	runOnce(t, stub)
	stub.set(map[int]int64{}, nil)
	runOnce(t, stub)
	if got := strings.Count(logs.String(), said); got != 2 {
		t.Errorf("the log has the line %d times after a second spell, want 2", got)
	}
}

func TestMemory_AProcessStillDyingIsNotKilledAgain(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	logs := captureLog(t)
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	a := applyOK(t, spec)

	// The entry a kill leaves, with the process still there: what the next run
	// finds of a process that takes a moment to go.
	killed.Store(killKeyOf(a), time.Now())
	stub := &memoryStub{sizes: map[int]int64{a.PID: 100 * mib}}
	runOnce(t, stub)

	if stub.calls() != 0 || !process.IsAlive(spec, a) || strings.Contains(logs.String(), "killed it") {
		t.Errorf("a process already killed was measured (%d calls), killed again, or logged:\n%s", stub.calls(), logs.String())
	}
}

func TestMemory_KillsAreRememberedForAMinuteAndByStartTime(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	a := applyOK(t, spec)
	waitServed(t, spec, a.PID)

	old, fresh := killKey{pid: 1, startedNs: 1}, killKey{pid: 2, startedNs: 2}
	killed.Store(old, time.Now().Add(-killedTTL-time.Second))
	killed.Store(fresh, time.Now().Add(-killedTTL+10*time.Second))
	// A reused PID: the same number, another process. Its exit is not a kill.
	other := killKey{pid: a.PID, startedNs: a.StartedAt.UnixNano() + 1}
	killed.Store(other, time.Now())

	runOnce(t, &memoryStub{})
	if _, ok := killed.Load(old); ok {
		t.Error("an entry older than a minute was kept")
	}
	if _, ok := killed.Load(fresh); !ok {
		t.Error("an entry younger than a minute was dropped")
	}

	if err := syscall.Kill(a.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the exit to be recorded", 5*time.Second, func() bool {
		rec, _ := loadCopyRecord(spec, 1)
		return rec.CrashCount == 1
	})
	if rec, _ := loadCopyRecord(spec, 1); rec.LastExitReason != "" {
		t.Errorf("an exit of a process that was not killed by the loop has the reason %q", rec.LastExitReason)
	}
	if _, ok := killed.Load(other); !ok {
		t.Error("the entry of another process was taken by this one's exit")
	}
}

func TestMemory_ARecordWhoseExitIsRecordedIsNotMeasured(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	a := applyOK(t, spec)

	// Its PID still exists (as a reused PID would), but the record says it
	// exited.
	unlock := state.Lock(spec.AppStateFile)
	a.LastExitAt = a.StartedAt.Add(time.Second)
	st, _ := state.Load(spec.AppStateFile)
	st.Put(a)
	err := state.Save(spec.AppStateFile, st)
	unlock()
	if err != nil {
		t.Fatal(err)
	}

	stub := &memoryStub{sizes: map[int]int64{a.PID: 100 * mib}}
	runOnce(t, stub)
	if stub.calls() != 0 || !process.IsAlive(spec, a) {
		t.Errorf("a record with its exit recorded was measured (%d calls) or its PID killed", stub.calls())
	}
}

func TestMemory_AStoppedAppIsNotMeasured(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	a := applyOK(t, spec)

	if err := appRegistry.Stop(spec.AppStateFile); err != nil {
		t.Fatal(err)
	}
	stub := &memoryStub{sizes: map[int]int64{a.PID: 100 * mib}}
	runOnce(t, stub)
	if stub.calls() != 0 || !process.IsAlive(spec, a) {
		t.Errorf("a stopped app was measured (%d calls) or killed", stub.calls())
	}
}

func TestMemory_ALimitThatIsNotASizeIsLoggedOnce(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	logs := captureLog(t)
	spec := copiesSpec(t, "app", 1, 0)
	spec.MaxMemory = "plenty" // Validate would refuse it; a registry written by hand may hold it
	cleanupApp(t, spec)
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}

	stub := &memoryStub{}
	runOnce(t, stub)
	runOnce(t, stub)
	if got := strings.Count(logs.String(), `memory: "app": max-memory "plenty" is not a size; no limit is enforced`); got != 1 {
		t.Errorf("the log has the line %d times, want 1:\n%s", got, logs.String())
	}
	if stub.calls() != 0 {
		t.Error("an app with no usable limit was measured")
	}
}

// An adopted copy has no exit-watcher: the tick counts its exit, and the kill
// is told from a crash by the note the loop left.
func TestMemory_AnAdoptedCopyIsCountedAsOOM(t *testing.T) {
	for _, seenBefore := range []bool{true, false} {
		t.Run(fmt.Sprintf("seen first: %v", seenBefore), func(t *testing.T) {
			useFakeRscript(t)
			resetMemoryState(t)
			spec := limitedSpec(t, "app", 1, 0)
			a := startCopy(t, spec)
			cleanupApp(t, spec)
			t.Cleanup(func() { killIfAlive(spec, a) })
			if seenBefore {
				reconcileApp(spec.AppStateFile)
			}

			stub := &memoryStub{sizes: map[int]int64{a.PID: 100 * mib}}
			runOnce(t, stub)
			waitGone(t, a)

			reconcileApp(spec.AppStateFile)
			rec, _ := loadCopyRecord(spec, 1)
			if rec.CrashCount != 1 || !rec.LastExitFailed || rec.LastExitReason != "oom" {
				t.Errorf("record = %+v, want one failed exit with the reason oom", rec)
			}
		})
	}
}

func TestMemory_AStrangersPIDIsNeverSignalledOrMarkedSeen(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}

	// The record names a PID that now belongs to a stranger.
	stranger := exec.Command("sleep", "30")
	if err := stranger.Start(); err != nil {
		t.Fatal(err)
	}
	go stranger.Wait()
	t.Cleanup(func() { stranger.Process.Kill() })
	rec := state.AppState{Name: "app", PID: stranger.Process.Pid, Copy: 1, Port: 40123, EntrypointFile: spec.EntrypointFile, StartedAt: time.Now().Add(-time.Minute)}
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{rec}}); err != nil {
		t.Fatal(err)
	}

	stub := &memoryStub{sizes: map[int]int64{rec.PID: 100 * mib}}
	if pending := runOnce(t, stub); len(pending) != 0 {
		t.Errorf("the run asks for a reconcile of %+v", pending)
	}
	if err := syscall.Kill(rec.PID, 0); err != nil {
		t.Errorf("the stranger was signalled: %v", err)
	}
	if wasSeen(spec.AppStateFile, rec) {
		t.Error("a process that is not the app's was marked seen")
	}
	if _, ok := killed.Load(killKeyOf(rec)); ok {
		t.Error("a kill that did not happen was remembered")
	}
}

func TestMemory_ADrainingCopyIsKilledAndNothingRestartsIt(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	one := startCopy(t, spec)
	t.Cleanup(func() { killIfAlive(spec, one) })

	// A second copy of an older version, draining.
	two, _ := startRaw(t, spec, 2)
	t.Cleanup(func() { killIfAlive(spec, two) })
	two.DrainingSince = time.Now()
	st, _ := state.Load(spec.AppStateFile)
	st.Put(two)
	if err := state.Save(spec.AppStateFile, st); err != nil {
		t.Fatal(err)
	}
	waitReady(t, process.InternalHost, two.Port)
	reconcileApp(spec.AppStateFile)

	stub := &memoryStub{sizes: map[int]int64{one.PID: 10 * mib, two.PID: 100 * mib}}
	runOnce(t, stub)
	waitGone(t, two)
	reconcileApp(spec.AppStateFile)

	if _, ok := loadCopyRecord(spec, 2); ok {
		t.Errorf("the record of the draining copy is still there: %+v", loadCopies(t, spec))
	}
	time.Sleep(300 * time.Millisecond)
	if copies := loadCopies(t, spec); len(copies) != 1 || copies[0].PID != one.PID || copies[0].CrashCount != 0 {
		t.Errorf("copies = %+v, want only copy 1, untouched", copies)
	}
	if !process.IsAlive(spec, one) {
		t.Error("the other copy was stopped")
	}
}

// stopCopies removes a record under the lock before it stops the process, so
// a copy the loop killed meanwhile leaves no exit to count.
func TestMemory_AKillDuringAPlannedStopIsNoCrash(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := limitedSpec(t, "app", 2, 0)
	cleanupApp(t, spec)
	applyOK(t, spec)
	waitRoles(t, spec, 2, 0)
	recs := loadCopies(t, spec)
	victim, keep := recs[1], recs[0]

	unlock := state.Lock(spec.AppStateFile)
	st, _ := state.Load(spec.AppStateFile)
	st.Remove(victim.Copy)
	if err := state.Save(spec.AppStateFile, st); err != nil {
		unlock()
		t.Fatal(err)
	}
	killOverLimit(context.Background(), memoryCopy{spec: spec, limit: limitMB * mib, rec: victim, recs: recs}, 100*mib)
	waitGone(t, victim)
	unlock()

	waitUntil(t, "the exit-watcher to finish", 5*time.Second, func() bool {
		_, still := killed.Load(killKeyOf(victim))
		return !still
	})
	time.Sleep(200 * time.Millisecond)

	// A reconcile queued behind the lock, by a ready-watcher, may start copy 2
	// again once it is released; that is no crash. What matters is that the
	// kill left no exit to count against any copy, and the other copy is
	// untouched.
	copies := loadCopies(t, spec)
	for _, rec := range copies {
		if rec.CrashCount != 0 || rec.LastExitReason != "" {
			t.Errorf("copy %d = %+v, want no crash and no reason recorded", rec.Copy, rec)
		}
	}
	if rec, ok := loadCopyRecord(spec, keep.Copy); !ok || rec.PID != keep.PID || !process.IsAlive(spec, rec) {
		t.Errorf("copy %d = %+v, want it untouched and running", keep.Copy, rec)
	}
}

func TestMemory_LoopWaitsFourTimesAsLongAsARunTook(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	a := applyOK(t, spec)

	const run = 100 * time.Millisecond
	var mu sync.Mutex
	var starts []time.Time
	slow := func(pids []int) (map[int]int64, error) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(run)
		return map[int]int64{a.PID: 10 * mib}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runMemoryLoop(ctx, 10*time.Millisecond, slow)
		close(done)
	}()
	time.Sleep(1700 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not return when its context ended")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(starts) < 2 || len(starts) > 4 {
		t.Fatalf("the loop ran %d times in 1.7s, want 2 to 4 (a run takes %v and is followed by four times that)", len(starts), run)
	}
	for i := 1; i < len(starts); i++ {
		if gap := starts[i].Sub(starts[i-1]); gap < 5*run-20*time.Millisecond {
			t.Errorf("run %d began %v after the one before, want at least %v", i, gap, 5*run)
		}
	}
}

func TestMemory_ReplyCarriesTheLimitInBytes(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)

	if got := applyResponse(t, spec).MaxMemoryBytes; got != limitMB*mib {
		t.Errorf("apply: max-memory-bytes = %d, want %d", got, limitMB*mib)
	}
	if got := restartResponse(t, spec).MaxMemoryBytes; got != limitMB*mib {
		t.Errorf("restart: max-memory-bytes = %d, want %d", got, limitMB*mib)
	}
	spec.MaxMemory = ""
	if got := applyResponse(t, spec).MaxMemoryBytes; got != 0 {
		t.Errorf("apply without a limit: max-memory-bytes = %d, want 0", got)
	}
}

// holding is a Measure hook that says the given copies hold size each.
func holding(size int64, recs ...state.AppState) measureFunc {
	return func(pids []int) (map[int]int64, error) {
		out := map[int]int64{}
		for _, rec := range recs {
			if slices.Contains(pids, rec.PID) {
				out[rec.PID] = size
			}
		}
		return out, nil
	}
}

func TestMemory_ApplyNamesTheCopiesANewLimitWillKill(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := copiesSpec(t, "app", 2, 0)
	cleanupApp(t, spec)
	applyOK(t, spec)
	recs := waitCopies(t, spec, 2)
	useHooks(t, hooks{Measure: holding(100*mib, recs...)})

	// A limit the copies are under names nothing.
	spec.MaxMemory = "128M"
	if got := applyResponse(t, spec).Message; strings.Contains(got, "will be killed") {
		t.Errorf("an apply with a limit the copies are under said %q", got)
	}

	// New, and lower: each names both copies.
	spec.MaxMemory = "64M"
	want := `Updated "app" without a restart (manifest changed: max-memory); copies 1 and 2 already hold more than 64M and will be killed`
	if got := applyResponse(t, spec).Message; got != want {
		t.Errorf("a lower limit: %q, want %q", got, want)
	}

	// The same, and higher: nothing new to say.
	if got := applyResponse(t, spec).Message; strings.Contains(got, "will be killed") {
		t.Errorf("an apply that changed nothing said %q", got)
	}
	spec.MaxMemory = "96M"
	if got := applyResponse(t, spec).Message; strings.Contains(got, "will be killed") {
		t.Errorf("a higher limit said %q", got)
	}

	// Restart tells nothing of the kind.
	spec.MaxMemory = "64M"
	if got := restartResponse(t, spec).Message; strings.Contains(got, "will be killed") {
		t.Errorf("restart said %q", got)
	}
}

func TestMemory_ApplyNamesOnlyTheCopiesThatAreOver(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := copiesSpec(t, "app", 2, 0)
	cleanupApp(t, spec)
	applyOK(t, spec)
	recs := waitCopies(t, spec, 2)
	useHooks(t, hooks{Measure: holding(100*mib, recs[1])})

	spec.MaxMemory = "64M"
	want := `Updated "app" without a restart (manifest changed: max-memory); copy 2 already holds more than 64M and will be killed`
	if got := applyResponse(t, spec).Message; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
}

func TestMemory_ApplyOmitsACopyTheCycleReplaced(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	spec := copiesSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	a := applyOK(t, spec)
	waitServed(t, spec, a.PID)

	// The record the measure saw is not the one that is there when the reply
	// is made: a copy under a record of another start time.
	heavy := []heavyCopy{{copy: 1, key: killKey{pid: a.PID, startedNs: a.StartedAt.UnixNano() + 1}}}
	spec.MaxMemory = "64M"
	if got := killClause(spec, heavy); got != "" {
		t.Errorf("killClause = %q for a copy that was replaced, want none", got)
	}
	heavy[0].key = killKeyOf(a)
	if got := killClause(spec, heavy); got != "; copy 1 already holds more than 64M and will be killed" {
		t.Errorf("killClause = %q", got)
	}
}

// waitCopies waits until the app has n copies recorded and alive, and returns
// them.
func waitCopies(t *testing.T, spec manifest.AppSpec, n int) []state.AppState {
	t.Helper()
	var recs []state.AppState
	waitUntil(t, fmt.Sprintf("%d copies to run", n), 10*time.Second, func() bool {
		recs = loadCopiesNow(spec)
		return len(recs) == n && !slices.ContainsFunc(recs, func(r state.AppState) bool { return !process.IsAlive(spec, r) })
	})
	return recs
}

func loadCopiesNow(spec manifest.AppSpec) []state.AppState {
	st, err := state.Load(spec.AppStateFile)
	if err != nil {
		return nil
	}
	return st.Copies()
}

func TestMemory_AFirstApplyMeasuresNothing(t *testing.T) {
	useFakeRscript(t)
	resetMemoryState(t)
	useHooks(t, hooks{Measure: func([]int) (map[int]int64, error) {
		t.Error("an apply of an app nobody knew measured its copies")
		return nil, nil
	}})
	spec := limitedSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	if got := applyResponse(t, spec).Message; strings.Contains(got, "will be killed") {
		t.Errorf("a first apply said %q", got)
	}
}
