/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// handleStop marks spec's app as stopped *before* signaling the process,
// so that by the time the kill causes its exit-channel to close, the
// already-running exit-watcher goroutine's call into triggerReconcile is
// a no-op instead of an instant, unwanted restart.
func handleStop(w http.ResponseWriter, r *http.Request) {
	var spec manifest.AppSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := manifest.Validate(spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := appRegistry.Stop(spec.AppStateFile); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Serialize against any reconcile in flight for this app: without the
	// lock, a tick that loaded state just before this clears it would
	// Save a resurrected record over the top of the cleared one.
	unlock := state.Lock(spec.AppStateFile)
	defer unlock()

	currentState, err := state.Load(spec.AppStateFile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := process.Stop(currentState.App().PID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Nothing should be running now — clear the record rather than leave
	// a stale dead PID sitting in state.json.
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := api.ApplyResponse{
		Action:  reconcile.ActionStop,
		Message: fmt.Sprintf("Stopped %q", spec.AppName),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
