/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

// This file holds how the daemon hears of exits: the exit-watchers on
// processes it started or adopted from an earlier shinyctl-api, and its memory
// of which processes it has seen alive.

import (
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

// watched holds the PIDs of the processes this daemon has an exit-watcher on:
// started by this daemon run, with the watcher still to record their exit. A
// process that dies has its exit recorded by its watcher, which has to wait
// for the state lock a tick may be holding; a tick that finds the process
// gone before the watcher got there must leave the decision to it, or it
// starts the copy again without the exit ever being counted (a
// restart-policy never copy would come back). A process adopted from an
// earlier daemon has no watcher, and the tick records its exit itself.
var watched sync.Map

// adoptedWatched holds the PIDs of the copies from an earlier run that have a
// watcher of the kind watchAdopted starts. A PID is in it from the moment the
// watcher starts until the process has exited, so it has one entry per live
// process, never more.
var adoptedWatched sync.Map

// watchAdopted starts, once per process, a watcher that reconciles the app
// the moment rec's process exits, for a copy that this daemon did not start
// and so has no exit-watcher of its own (see watched). The watcher only
// triggers the reconcile: the exit is recorded as the tick records it, so a
// crash is counted exactly as without the watcher, once. The reconcile finds
// the copy gone and promotes a warm copy at once instead of at the next tick.
// Where the system can't watch a process that is not our child, the tick
// stays the only check, and that is logged, once for the whole run.
func watchAdopted(spec manifest.AppSpec, rec state.AppState) {
	pid := rec.PID
	if isWatched(pid) {
		return
	}
	if _, loaded := adoptedWatched.LoadOrStore(pid, struct{}{}); loaded {
		return
	}
	exited, err := currentHooks().WatchExit(spec, rec)
	if err != nil {
		adoptedWatched.Delete(pid)
		if !errors.Is(err, process.ErrNotOurs) {
			noWatchOnce.Do(func() {
				log.Printf("reconcile: this system can't watch processes started by an earlier run (%v); they are checked every %s instead", err, reconcileInterval)
			})
		}
		return
	}
	stateFile := spec.AppStateFile
	go func() {
		<-exited
		adoptedWatched.Delete(pid)
		triggerReconcile(stateFile)
	}()
}

var noWatchOnce sync.Once

// watchRestoredCopies gives every copy that is running when the daemon
// starts its watcher now, so that a copy from the earlier run that dies
// during the first seconds is noticed at once too, not at the first tick.
// It also marks them seen, which is what lets the reconcile that follows
// count their exit as a crash. It is called when the load balancer is ready
// (started or adopted), the moment the daemon can act on an exit. It does not
// take the state lock: it only reads the state files, which are replaced whole
// and never edited in place, as refreshPools does, and watchAdopted checks
// each process itself. A record that a cycle replaces meanwhile costs, at
// worst, a watcher on the old process, whose exit is a reconcile that finds
// nothing to do.
func watchRestoredCopies() {
	// What an earlier shinyctl-api left the load balancer serving: those
	// copies are the copies of this machine, whatever a lookup says, so a
	// misjudged owner can't stop and move one that has visitors.
	listed := gateway.Listed()
	for _, spec := range appRegistry.All() {
		if !appRegistry.Managed(spec.AppStateFile) {
			continue
		}
		st, err := state.Load(spec.AppStateFile)
		if err != nil {
			continue
		}
		recs := st.Copies()
		for i, up := range process.AliveAll(spec, recs) {
			if up {
				markSeen(spec.AppStateFile, recs[i])
				watchAdopted(spec, recs[i])
				if recs[i].Copy >= 1 && slices.Contains(allDials(listed[spec.AppStateFile]), dialOf(recs[i])) {
					ownedPorts.Store(pidPort{recs[i].PID, recs[i].Port}, struct{}{})
				}
			}
		}
	}
}

func isWatched(pid int) bool {
	_, ok := watched.Load(pid)
	return ok
}

// seen is the process of each copy that this daemon has found alive, by
// state file and copy: one entry per copy, so it can't grow past the number
// of copies, and dropped when the copy or the app is forgotten. A process
// that is gone and was never seen alive here died while no daemon was
// looking (a reboot, or a daemon that was down), which is not a crash to
// count: the copy just starts, as it always has. Only a process seen alive
// and found dead later, with nobody having recorded its exit, is an unseen
// exit. The started-at time is part of the value so a reused PID can't match
// a different record.
var seen struct {
	sync.Mutex
	m map[seenKey]seenProcess
}

type seenKey struct {
	stateFile string
	copy      int
}

type seenProcess struct {
	pid     int
	started time.Time
}

func markSeen(stateFile string, app state.AppState) {
	seen.Lock()
	defer seen.Unlock()
	if seen.m == nil {
		seen.m = make(map[seenKey]seenProcess)
	}
	seen.m[seenKey{stateFile, app.Copy}] = seenProcess{app.PID, app.StartedAt}
}

func wasSeen(stateFile string, app state.AppState) bool {
	seen.Lock()
	defer seen.Unlock()
	return seen.m[seenKey{stateFile, app.Copy}] == seenProcess{app.PID, app.StartedAt}
}

// forgetSeen drops the entry of one copy: its exit has been counted, or it
// is gone.
func forgetSeen(stateFile string, copy int) {
	seen.Lock()
	defer seen.Unlock()
	delete(seen.m, seenKey{stateFile, copy})
}

// forgetApp drops the entries of every copy of the app at stateFile.
func forgetApp(stateFile string) {
	seen.Lock()
	defer seen.Unlock()
	for key := range seen.m {
		if key.stateFile == stateFile {
			delete(seen.m, key)
		}
	}
}
