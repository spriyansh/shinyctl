/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"net/http"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

// handleApply is the server side of `shinyctl apply`: the CLI only loads,
// validates and POSTs the manifest, then prints the response — the
// decide-and-act work happens here, in the one process that owns
// state.json.
//
// It also records spec in appRegistry, so the background reconcile loop
// has something to check on its own even when no client is asking.
func handleApply(w http.ResponseWriter, r *http.Request) {
	var spec manifest.AppSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := manifest.Validate(spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// A registry we can't persist is an app the next shinyctl-api won't
	// know about — fail the apply rather than start something we'd forget.
	if err := appRegistry.Remember(spec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp, err := reconcileSpec(spec, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
