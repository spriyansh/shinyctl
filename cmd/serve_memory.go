/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

// This file holds the loop that keeps the copies of apps with a max-memory
// under it: it measures them about once a second and kills a copy that holds
// more, so the warm copy takes over before the next visitor comes.

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// memoryInterval is how often the copies of apps with a limit are measured:
// the shortest gap between two runs, which grows when a run is slow (see
// runMemoryLoop).
const memoryInterval = time.Second

// killedTTL is how long a kill is remembered for the exit that follows it. An
// exit seen later than that is a crash like any other, which is the price of
// an entry that is certain to go away.
const killedTTL = 60 * time.Second

// killReapWait is how long the loop waits for a copy it killed to be gone
// before it reconciles, for a copy that has no watcher to do so.
const killReapWait = 2 * time.Second

// killKey names one process: a PID alone could be reused, and the start time
// is in nanoseconds because a time.Time carries a monotonic reading that
// would make two keys for one process.
type killKey struct {
	pid       int
	startedNs int64
}

func killKeyOf(rec state.AppState) killKey {
	return killKey{rec.PID, rec.StartedAt.UnixNano()}
}

// killed holds the processes the loop has killed for going over their limit,
// each with the time of the kill. The exit-watcher of a copy this daemon
// started, and observe for one it adopted, take their entry to tell that kill
// from a crash. An entry also keeps the loop from killing a process that is
// still dying at its next run.
var killed sync.Map

// memorySpells remembers what the loop has already said, so that a problem
// that lasts is in the log once and not every second: a measurement that
// fails, a copy that can't be measured, a limit that is not a size.
var memorySpells struct {
	sync.Mutex
	failed     bool
	unmeasured map[killKey]bool
	unparsable map[string]bool
}

// measureFunc is process.ExactMemory: the memory of each PID, absent for one
// it could not measure.
type measureFunc func(pids []int) (map[int]int64, error)

// runMemoryLoop runs enforceMemoryOnce until ctx ends. The next run comes
// interval after the last, or four times as long as the last took if that is
// more: measuring costs CPU, on macOS about three times the time it takes, and
// a machine that is slow at it has the most else to do. The interval and the
// measure are parameters, not variables, because the tests run this loop
// beside the daemon's goroutines and must not swap what they read.
func runMemoryLoop(ctx context.Context, interval time.Duration, measure measureFunc) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		started := time.Now()
		for _, p := range enforceMemoryOnce(ctx, measure) {
			go reconcileWhenGone(p)
		}
		timer.Reset(max(interval, 4*time.Since(started)))
	}
}

// unwatchedKill is a copy the loop killed that nobody is waiting for the exit
// of: the daemon did not start it and no watcher of an adopted copy is on it.
type unwatchedKill struct {
	stateFile string
	app       state.AppState
}

// reconcileWhenGone reconciles the app of p once the process is gone, or after
// killReapWait if it is not: the cycle should find it dead, not dying.
func reconcileWhenGone(p unwatchedKill) {
	deadline := time.Now().Add(killReapWait)
	for process.PIDRunning(p.app) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	triggerReconcile(p.stateFile)
}

// memoryCopy is a copy that is measured.
type memoryCopy struct {
	spec  manifest.AppSpec
	limit int64
	rec   state.AppState
	recs  []state.AppState // every copy of the app, to name the ones in a pool
}

// enforceMemoryOnce is one run of the loop: it measures, with one call, every
// copy that is running under a limit, and kills those over it. The copies that
// were killed and are not watched are returned, for the caller to reconcile
// when they are gone.
//
// Nothing is measured unless a managed app has a limit, and then only its
// copies. The state files are read without their lock: they are replaced
// whole, never edited in place, so a read sees one record or the next. A
// record that a cycle replaces while the run is measuring costs, at worst, a
// kill that finds another process at the PID, which Kill declines.
func enforceMemoryOnce(ctx context.Context, measure measureFunc) []unwatchedKill {
	now := time.Now()
	killed.Range(func(k, at any) bool {
		if now.Sub(at.(time.Time)) > killedTTL {
			killed.Delete(k)
		}
		return true
	})

	var copies []memoryCopy
	var pids []int
	for _, listed := range appRegistry.All() {
		stateFile := listed.AppStateFile
		spec, ok := appRegistry.ManagedSpec(stateFile)
		if !ok {
			continue
		}
		limit := spec.EffectiveMaxMemory()
		if limit == 0 {
			noteUnparsable(spec)
			continue
		}
		st, err := state.Load(stateFile)
		if err != nil {
			continue
		}
		recs := st.Copies()
		for _, rec := range runningCopies(recs) {
			copies = append(copies, memoryCopy{spec, limit, rec, recs})
			pids = append(pids, rec.PID)
		}
	}
	if len(pids) == 0 {
		return nil
	}

	sizes, err := measure(pids)

	memorySpells.Lock()
	if err != nil {
		if !memorySpells.failed {
			log.Printf("memory: can't measure the copies: %v; no limit is enforced until they can be", err)
		}
		memorySpells.failed = true
		memorySpells.Unlock()
		return nil
	}
	memorySpells.failed = false
	unmeasured := map[killKey]bool{}
	for _, c := range copies {
		if _, ok := sizes[c.rec.PID]; ok {
			continue
		}
		key := killKeyOf(c.rec)
		if !memorySpells.unmeasured[key] {
			log.Printf("memory: can't measure copy %d (pid %d) of %q; its limit is not enforced until it can be", c.rec.Copy, c.rec.PID, c.spec.AppName)
		}
		unmeasured[key] = true
	}
	memorySpells.unmeasured = unmeasured
	memorySpells.Unlock()

	var pending []unwatchedKill
	for _, c := range copies {
		if size, ok := sizes[c.rec.PID]; ok && size > c.limit {
			if p, unwatched := killOverLimit(ctx, c, size); unwatched {
				pending = append(pending, p)
			}
		}
	}
	return pending
}

// runningCopies picks from recs the copies whose memory is worth measuring:
// those that are running and that nobody has killed yet.
func runningCopies(recs []state.AppState) []state.AppState {
	var running []state.AppState
	for _, rec := range recs {
		// A record whose exit is recorded is not running, whatever is at its
		// PID now. This takes the clock not to have stepped back, as observe
		// does.
		if rec.Copy < 1 || !rec.LastExitAt.Before(rec.StartedAt) || !process.PIDRunning(rec) {
			continue
		}
		if _, dying := killed.Load(killKeyOf(rec)); dying {
			continue
		}
		running = append(running, rec)
	}
	return running
}

// noteUnparsable says, once per app and value, that a limit in the registry is
// not a size and so is not enforced. Validate refuses one, so this is a
// registry written by hand.
func noteUnparsable(spec manifest.AppSpec) {
	if _, err := manifest.ParseSize(spec.MaxMemory); spec.MaxMemory == "" || err == nil {
		return // unset or 0 is no limit, and says so by itself
	}
	id := spec.AppStateFile + "\x00" + spec.MaxMemory
	memorySpells.Lock()
	defer memorySpells.Unlock()
	if memorySpells.unparsable[id] {
		return
	}
	if memorySpells.unparsable == nil {
		memorySpells.unparsable = map[string]bool{}
	}
	memorySpells.unparsable[id] = true
	log.Printf("memory: %q: max-memory %q is not a size; no limit is enforced", spec.AppName, spec.MaxMemory)
}

// killOverLimit kills copy c, which holds size, and has the warm copy take its
// place in the same step. The second result says that nobody is watching the
// process, so the caller must reconcile when it is gone.
//
// The order matters. The kill is noted before the signal, so that whoever
// sees the exit finds the note; the process is recognised before it is marked
// seen, so that a PID that now belongs to a stranger is never marked; and the
// pool changes right after the signal, before any exit is recorded, because
// the reconcile that follows may be a backoff away from starting a
// replacement and visitors must not wait for it.
func killOverLimit(ctx context.Context, c memoryCopy, size int64) (unwatchedKill, bool) {
	stateFile := c.spec.AppStateFile
	if ctx.Err() != nil || !appRegistry.Managed(stateFile) {
		return unwatchedKill{}, false
	}

	key := killKeyOf(c.rec)
	killed.Store(key, time.Now())
	signaled, err := process.Kill(c.rec, func() {
		if !isWatched(c.rec.PID) {
			markSeen(stateFile, c.rec)
		}
	}, identityFallbacks(c.spec)...)
	if err != nil {
		log.Printf("memory: %q: killing copy %d (pid %d): %v", c.spec.AppName, c.rec.Copy, c.rec.PID, err)
	}
	if !signaled {
		killed.Delete(key)
		return unwatchedKill{}, false
	}

	line := fmt.Sprintf("memory: %q: copy %d (pid %d) holds %s, over max-memory %s; killed it", c.spec.AppName, c.rec.Copy, c.rec.PID, formatMem(size), formatMem(c.limit))
	if pool, changed := gateway.Withdraw(stateFile, dialOf(c.rec)); changed {
		// The next reconcile's roles line stays silent, as it finds this pool
		// already in place, so this is the line that says who took over.
		copyOf := func(dials []string) []int {
			var nums []int
			for _, dial := range dials {
				for _, rec := range c.recs {
					if dialOf(rec) == dial {
						nums = append(nums, rec.Copy)
						break
					}
				}
			}
			return nums
		}
		line += "; " + reconcile.DescribeRoles(copyOf(pool.Active), copyOf(pool.Warm), copyOf(pool.Draining))
	}
	log.Print(line)

	markKilled(c, size)

	_, adopted := adoptedWatched.Load(c.rec.PID)
	if isWatched(c.rec.PID) || adopted {
		return unwatchedKill{}, false
	}
	return unwatchedKill{stateFile, c.rec}, true
}

// markKilled writes the kill into the copy's log, so that someone reading it
// finds why the copy stops there.
func markKilled(c memoryCopy, size int64) {
	logPath, err := process.CopyLogPath(c.spec, c.rec.Copy)
	if err != nil {
		return
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "--- shinyctl: killed copy %d at %s: %s over max-memory %s ---\n", c.rec.Copy, time.Now().Format(time.RFC3339), formatMem(size), formatMem(c.limit))
}
