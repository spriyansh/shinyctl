/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

// shuttingDown is set once, when /shutdown is accepted, and never cleared by
// the daemon: it exits. It gates everything that would start a process or
// push a table to the load balancer while the apps and the load balancer are
// being stopped.
var shuttingDown atomic.Bool

var errShuttingDown = errors.New("shinyctl-api is shutting down")

// shutdownDeps are what /shutdown does besides stopping the apps, so tests
// can leave the process's load balancer and loops alone.
type shutdownDeps struct {
	unit         func() (unit string, user bool) // the systemd service this daemon is the main process of, or ""; user says it is a user service
	stopLoops    func()                          // stops the loops and the load balancer's supervisor, and waits for it
	stopBalancer func() (bool, error)            // stops the load balancer through its lock
	exit         func()                          // asks serve to return, without blocking
}

// shutdownHandler stops every app's copies and the load balancer, answers,
// and asks the daemon to exit. Errors are reported in the answer and do not
// stop it: the loops are already cancelled and the daemon cannot carry on.
func shutdownHandler(d shutdownDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}

		// A service restarts the daemon, and with it every app, two seconds
		// after it exits: nothing is stopped, and the user is told how.
		if unit, user := d.unit(); unit != "" {
			http.Error(w, fmt.Sprintf("it runs as the systemd service %s, which would start it and every app again; run %s, then shinyctl shutdown again",
				unit, systemctlStop(unit, user)), http.StatusConflict)
			return
		}

		if !shuttingDown.CompareAndSwap(false, true) {
			http.Error(w, "it is already shutting down", http.StatusConflict)
			return
		}
		log.Print("shutdown: stopping every app and the load balancer")

		// The flag is set first: whatever reconciles from here on is gated,
		// and a cycle that passed the gate earlier holds its app's state
		// lock, which the stops below wait for and then undo.
		d.stopLoops()

		var resp api.ShutdownResponse
		resp.PID = os.Getpid()

		specs := appRegistry.All()
		sort.Slice(specs, func(i, j int) bool {
			if specs[i].AppName != specs[j].AppName {
				return specs[i].AppName < specs[j].AppName
			}
			return specs[i].AppStateFile < specs[j].AppStateFile
		})
		outcomes := make([]appShutdown, len(specs))
		var wg sync.WaitGroup
		for i, spec := range specs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				outcomes[i] = shutdownApp(spec)
			}()
		}
		wg.Wait()
		for _, o := range outcomes {
			if o.stopped {
				resp.Apps = append(resp.Apps, o.name)
			}
			resp.Notes = append(resp.Notes, o.notes...)
			resp.Problems = append(resp.Problems, o.problems...)
		}

		stopped, err := d.stopBalancer()
		resp.Balancer = stopped
		if err != nil {
			resp.Problems = append(resp.Problems, fmt.Sprintf("the load balancer: %v", err))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		d.exit()
	}
}

// systemctlStop is the command that stops unit: a user service, which is
// where the daemon of a user's shinyctl runs, needs --user.
func systemctlStop(unit string, user bool) string {
	if user {
		return "systemctl --user stop " + unit
	}
	return "systemctl stop " + unit
}

// appShutdown is what stopping one app came to.
type appShutdown struct {
	name     string
	stopped  bool // at least one copy was ours and has been stopped
	notes    []string
	problems []string
}

// shutdownApp stops every recorded copy of spec's app, under its state lock,
// and clears the records of the copies that stopped. The app stays
// registered and managed: shutdown is power off, not delete. A copy that
// could not be stopped keeps its record, so running shutdown again retries.
func shutdownApp(spec manifest.AppSpec) appShutdown {
	out := appShutdown{name: spec.AppName}

	unlock := state.Lock(spec.AppStateFile)
	defer unlock()

	st, err := state.Load(spec.AppStateFile)
	if err != nil {
		out.problems = append(out.problems, fmt.Sprintf("%q: %v", spec.AppName, err))
		return out
	}
	recs := st.Copies()
	if len(recs) == 0 {
		return out
	}

	results := stopEach("shutdown", spec, recs)
	for i, r := range results {
		out.stopped = out.stopped || r.signaled
		if r.note != "" {
			out.notes = append(out.notes, fmt.Sprintf("%q%s", spec.AppName, r.note))
		}
		if r.err != nil {
			out.problems = append(out.problems, fmt.Sprintf("%q: stopping copy %d (pid %d): %v", spec.AppName, copyNumber(recs[i], i), recs[i].PID, r.err))
		}
	}

	if err := state.Save(spec.AppStateFile, keptState(spec.AppGroup, st, results)); err != nil {
		out.problems = append(out.problems, fmt.Sprintf("%q: %v", spec.AppName, err))
	}
	return out
}

// copyNumber is the number to name a copy by: its recorded one, or its place
// in the list for a record from before copies were numbered.
func copyNumber(rec state.AppState, i int) int {
	if rec.Copy > 0 {
		return rec.Copy
	}
	return i + 1
}

// keptState is what is saved after the stops: the records that are kept, and
// the revision, so a kept copy that was due a restart still counts as outdated.
func keptState(group string, st state.State, results []copyStop) state.State {
	return state.State{AppGroup: group, Revision: st.Revision, Apps: keptRecords(st.Copies(), results)}
}

// keptRecords are the records of the copies that could not be stopped, as
// stopCopies keeps them; the rest are dropped.
func keptRecords(recs []state.AppState, results []copyStop) []state.AppState {
	var kept []state.AppState
	for i, rec := range recs {
		if results[i].err != nil {
			kept = append(kept, rec)
		}
	}
	return kept
}
