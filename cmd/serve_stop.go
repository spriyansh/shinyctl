/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"log"
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
// a no-op instead of an instant, unwanted restart. It signals the recorded
// process only if that process is still this app.
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

	note, err := stopRecorded("stop", spec, currentState.App())
	if err != nil {
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
		Message: fmt.Sprintf("Stopped %q%s", spec.AppName, note),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// identityFallbacks lists the entrypoints a record without a recorded one
// may be matched against: the request's, and the one the registry last
// applied. The manifest may have been edited since the running copy was
// applied, and the registry holds what that copy was most likely started
// from. A delete request may carry no entrypoint at all.
func identityFallbacks(spec manifest.AppSpec) []string {
	fallbacks := []string{spec.EntrypointFile}
	if applied, ok := appRegistry.Spec(spec.AppStateFile); ok && applied.EntrypointFile != spec.EntrypointFile {
		fallbacks = append(fallbacks, applied.EntrypointFile)
	}
	return fallbacks
}

// stopRecorded stops app's recorded process for stop and delete, signaling
// it only while it is identifiably this app's. If the recorded PID turns
// out to be dead or someone else's (a reused PID), nothing is signaled and
// the returned note says which PID was left alone; the caller carries on:
// a process that can't be identified must not be signaled, and the user
// asked for the app to be down. The caller holds the state lock.
func stopRecorded(op string, spec manifest.AppSpec, app state.AppState) (note string, err error) {
	signaled, err := process.Stop(app, identityFallbacks(spec)...)
	if err != nil {
		return "", err
	}
	if signaled || app.PID <= 0 {
		return "", nil
	}

	log.Printf("%s: %q: recorded pid %d is not running this app; not signaling it", op, spec.AppName, app.PID)
	return fmt.Sprintf(" (recorded pid %d is not running this app, so it was not signaled)", app.PID), nil
}
