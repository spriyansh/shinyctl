/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// handleStop marks spec's app as stopped *before* signaling the processes,
// so that by the time the kill causes their exit-channels to close, the
// already-running exit-watcher goroutines' calls into triggerReconcile are
// no-ops instead of instant, unwanted restarts. It signals each recorded
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

	note, err := stopAll("stop", spec, currentState.Copies())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Nothing should be running now — clear the records rather than leave
	// stale dead PIDs sitting in state.json.
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	forgetApp(spec.AppStateFile)
	clearNotStarted(spec.AppStateFile)

	// The app is unmanaged now, so its port closes and its pool is dropped. A
	// failure here is the
	// load balancer's to retry on the next tick; the stop itself is done.
	if err := syncProxy(""); err != nil {
		log.Printf("stop: %q: %v", spec.AppName, err)
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

// stopRecorded stops app's recorded process for stop, delete and restart,
// signaling it only while it is identifiably this app's. If the recorded
// PID is someone else's (a reused PID), nothing is signaled and the
// returned note says which PID was left alone; the caller carries on: a
// process that can't be identified must not be signaled, and the user
// asked for the app to be down. A recorded PID that isn't running at all
// is the ordinary case of an app that already exited, so it gets no note.
// signaled says whether the process was ours and has been stopped. The
// caller holds the state lock.
func stopRecorded(op string, spec manifest.AppSpec, app state.AppState) (signaled bool, note string, err error) {
	signaled, err = process.Stop(app, identityFallbacks(spec)...)
	if err != nil {
		return false, "", err
	}
	if signaled || !process.PIDRunning(app) {
		return signaled, "", nil
	}

	log.Printf("%s: %q: recorded pid %d is not running this app; not signaling it", op, spec.AppName, app.PID)
	return false, fmt.Sprintf(" (recorded pid %d is not running this app, so it was not signaled)", app.PID), nil
}

// stopAll is stopRecorded for every copy at once: they are stopped in
// parallel, since each may take up to ten seconds to answer SIGTERM, and the
// notes come back in copy order. The first error is returned after all have
// been tried.
func stopAll(op string, spec manifest.AppSpec, recs []state.AppState) (notes string, err error) {
	results := make([]string, len(recs))
	errs := make([]error, len(recs))
	var wg sync.WaitGroup
	for i, rec := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i], errs[i] = stopRecorded(op, spec, rec)
		}()
	}
	wg.Wait()

	for i := range recs {
		if errs[i] != nil {
			return "", errs[i]
		}
		notes += results[i]
	}
	return notes, nil
}
