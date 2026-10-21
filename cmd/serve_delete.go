/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// handleDelete is stop, then forget: kill the process if it's running,
// drop the app from the registry (spec and stopped flag both), and remove
// its state file — shinyctl is the only thing that ever wrote it, so
// forgetting the app means removing it, not leaving a stale record behind.
//
// Ordering matters the same way it does for stop: the registry is marked
// stopped *before* the kill, so the exit-watcher goroutine that fires when
// the process dies sees an unmanaged app and does nothing. Forget then
// keeps it that way — Managed is false for unknown apps too. Idempotent:
// deleting an app that's already stopped, or never applied, succeeds. The
// recorded process is signaled only if it is still this app, matched
// against the applied spec when the request carries no entrypoint.
func handleDelete(w http.ResponseWriter, r *http.Request) {
	var spec manifest.AppSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Not manifest.Validate — see cmd/delete.go for why.
	if spec.AppName == "" || spec.AppStateFile == "" {
		http.Error(w, "app-name and app-state-file are required", http.StatusBadRequest)
		return
	}

	if err := appRegistry.Stop(spec.AppStateFile); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	unlock := state.Lock(spec.AppStateFile)
	defer unlock()

	currentState, err := state.Load(spec.AppStateFile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Before Forget: identityFallbacks reads the applied spec.
	_, note, err := stopRecorded("delete", spec, currentState.App())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := appRegistry.Forget(spec.AppStateFile); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := os.Remove(spec.AppStateFile); err != nil && !os.IsNotExist(err) {
		http.Error(w, fmt.Sprintf("removing %s: %v", spec.AppStateFile, err), http.StatusInternalServerError)
		return
	}

	resp := api.ApplyResponse{
		Action:  reconcile.ActionDelete,
		Message: fmt.Sprintf("Deleted %q — stopped, forgotten, and %s removed%s", spec.AppName, spec.AppStateFile, note),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
