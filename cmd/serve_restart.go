/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// handleRestart makes the app's copies pick up new code, which a re-apply
// never notices: apply compares only the manifest, and replaces the copies of
// an app whose manifest changed. It raises the state's revision, which makes
// every running copy outdated, and reconciles: a full set of new copies starts
// next to the running ones on new internal ports, and the request returns at
// once. The old copies keep serving until enough new ones are ready; then new
// visitors go to the new copies and the old ones drain (see
// reconcile.DecideCopies), so nobody is cut off and, since the new copies
// start before anything stops, nobody waits for R. It also starts an app that
// is stopped, never applied, or crash-looping, right away and with a clean
// crash history, the same as an explicit apply.
//
// The order matters; each step is here for a reason:
//   - Validate first, so a bad manifest fails with nothing touched.
//   - checkMove next, for the same reason: a restart onto a port the load
//     balancer can't take is refused (409) before anything is stopped, so
//     the app keeps serving where it was.
//   - The state lock before Remember: Remember clears the stopped flag, and
//     a tick running in the gap between the two would start the app, only
//     for the revision below to make that fresh copy outdated at once.
//     Taking the registry's mutex inside the state lock is safe, as it
//     never takes a state lock.
//   - Remember before the revision is raised, as apply does: if the registry
//     can't be persisted the request fails before anything changes.
//   - The revision is saved before the reconcile, so the copies it starts
//     are of the new revision and the running ones are outdated.
//   - syncProxy last, which follows a port or host change in the request.
//     The new copies need no reload: the table is pushed to the load
//     balancer as the copies become ready.
func handleRestart(w http.ResponseWriter, r *http.Request) {
	var spec manifest.AppSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Restart remembers the spec and may start a never-applied app, so it
	// refuses what an apply refuses.
	if err := manifest.CheckPathCharacters(spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := manifest.Validate(spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	unlock := state.Lock(spec.AppStateFile)
	defer unlock()

	if err := checkMove(spec); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	previous, known := appRegistry.Spec(spec.AppStateFile)
	_, wasServed := servedApp(spec.AppStateFile)

	if err := appRegistry.Remember(spec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	currentState, err := state.Load(spec.AppStateFile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// A recorded PID that is running something else is left alone, and said
	// so, as stop and delete do.
	var notes string
	recs := currentState.Copies()
	for i, alive := range process.AliveAll(spec, recs) {
		if !alive && process.PIDRunning(recs[i]) {
			log.Printf("restart: %q: recorded pid %d is not running this app; not signaling it", spec.AppName, recs[i].PID)
			notes += fmt.Sprintf(" (recorded pid %d is not running this app, so it was not signaled)", recs[i].PID)
		}
	}

	currentState.Revision++
	if err := state.Save(spec.AppStateFile, currentState); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp, err := reconcileLocked(spec, reconcileOpts{explicit: true, skipDrain: r.URL.Query().Get("now") == "1"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if status, err := syncMoved(spec, previous, known, wasServed); err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	resp.Message += notes
	resp.Action = reconcile.ActionRestart
	resp.MaxMemoryBytes = spec.EffectiveMaxMemory()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
