/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"net/http"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// handleRestart stops the app's process, if it is running and still this
// app's, and starts it again from spec. It is the way to make a running
// app pick up new code or manifest edits, which a re-apply doesn't do: the
// reconcile only acts on a port or name change. It also starts an app that
// is stopped, never applied, or crash-looping, right away and with a clean
// crash history, the same as an explicit apply.
//
// The order matters; each step is here for a reason:
//   - Validate first, so a bad manifest fails with nothing touched.
//   - The state lock before Remember: Remember clears the stopped flag, and
//     a tick running in the gap between the two would start the app, only
//     for the stop below to kill that fresh copy. Taking the registry's
//     mutex inside the state lock is safe, as it never takes a state lock.
//   - Remember before the stop, as apply does: if the registry can't be
//     persisted the request fails before anything is stopped. The cost is
//     that identityFallbacks then holds only this request's entrypoint,
//     which matters just for a record from before entrypoints were
//     recorded whose entrypoint also changes in this restart: it is left
//     unsignaled, with a note, and the port check fails.
//   - stopRecorded, for the same identity rules as stop and delete. It
//     returns only once the process is gone, so its port is free.
//   - A zero record, saved before the start. It clears the crash history
//     (restart is an explicit "run this now"), makes a failed start record
//     exactly one crash with no stale PID, and means the old process's
//     exit-watcher, blocked on the lock, finds a PID that isn't its own and
//     records nothing.
//   - reconcileLocked with that zero record always decides Start, so the
//     port check, launch, exit-watcher and Save are the ones apply uses,
//     and failures are recorded as crashes the same way.
func handleRestart(w http.ResponseWriter, r *http.Request) {
	var spec manifest.AppSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := manifest.Validate(spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	unlock := state.Lock(spec.AppStateFile)
	defer unlock()

	if err := appRegistry.Remember(spec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	currentState, err := state.Load(spec.AppStateFile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	app := currentState.App()

	signaled, note, err := stopRecorded("restart", spec, app)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp, err := reconcileLocked(spec, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if signaled {
		resp.Message += previousInstanceNote(app)
	} else {
		resp.Message += note
	}
	resp.Action = reconcile.ActionRestart

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
