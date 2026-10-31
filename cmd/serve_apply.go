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
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// handleApply is the server side of `shinyctl apply`: the CLI only loads,
// validates and POSTs the manifest, then prints the response — the
// decide-and-act work happens here, in the one process that owns
// state.json. A manifest change that shapes the process (env, working-dir,
// app-group, app-name, entrypoint-file) is a redeploy, as restart is: new
// copies start next to the running ones, which drain once the new ones are
// ready; a change to replicas or warm-replicas starts copies, or drains the
// ones it removes; the other fields take effect without touching a process.
//
// It also records spec in appRegistry, so the background reconcile loop
// has something to check on its own even when no client is asking. The
// loop compares a running process with the registry's spec, so a tick
// between recording the spec and reconciling it could do the replacement
// itself, and this apply would answer "Nothing to change" to an edit. The
// state lock is therefore taken before the registry is updated and held
// through the reconcile, the same cycle a tick runs under. The registry's
// own mutex is a leaf (it never takes a state lock), so taking it inside
// the state lock is safe. So is the load balancer's: syncing never takes a
// state lock either.
func handleApply(w http.ResponseWriter, r *http.Request) {
	var spec manifest.AppSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Only apply and restart refuse these: stop and delete must still work for
	// an app an earlier version accepted.
	if err := manifest.CheckPathCharacters(spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := manifest.Validate(spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// A limit that is new or lower than the applied one kills the copies that
	// already hold more, within the second. They are looked for before the
	// lock is taken: measuring can take seconds when the machine is slow, and
	// the lock is not for that.
	var heavy []heavyCopy
	if limit := spec.EffectiveMaxMemory(); limit > 0 {
		if applied, known := appRegistry.Spec(spec.AppStateFile); known {
			if was := applied.EffectiveMaxMemory(); was == 0 || limit < was {
				heavy = copiesOver(spec, limit)
			}
		}
	}

	unlock := state.Lock(spec.AppStateFile)
	defer unlock()

	// Before anything changes: a move onto a port the load balancer can't
	// take leaves the app serving where it is, with the old manifest.
	if err := checkMove(spec); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	// Read before Remember overwrites it, to tell the user about changes
	// that need no restart, and to put it back if a move fails.
	previous, known := appRegistry.Spec(spec.AppStateFile)
	_, wasServed := servedApp(spec.AppStateFile)

	// A registry we can't persist is an app the next shinyctl-api won't
	// know about — fail the apply rather than start something we'd forget.
	if err := appRegistry.Remember(spec); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var settings []string
	if known {
		settings = reconcile.SettingsChanged(previous, spec)
	}

	resp, err := reconcileLocked(spec, reconcileOpts{explicit: true, settings: settings, skipDrain: r.URL.Query().Get("now") == "1"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Whatever the reconcile did, the load balancer follows: a new process,
	// or a port or host that moved with no restart at all.
	if status, err := syncMoved(spec, previous, known, wasServed); err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	resp.MaxMemoryBytes = spec.EffectiveMaxMemory()
	resp.Message += killClause(spec, heavy)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// heavyCopy is a copy that holds more than a new max-memory allows, as the
// process it was when it was measured.
type heavyCopy struct {
	copy int
	key  killKey
}

// copiesOver measures the running copies of spec's app and returns those
// holding more than limit. A measure that fails finds none: the loop says so
// in the log, and the apply goes on without the warning.
func copiesOver(spec manifest.AppSpec, limit int64) []heavyCopy {
	st, err := state.Load(spec.AppStateFile)
	if err != nil {
		return nil
	}
	running := runningCopies(st.Copies())
	pids := make([]int, len(running))
	for i, rec := range running {
		pids[i] = rec.PID
	}
	if len(pids) == 0 {
		return nil
	}
	sizes, err := currentHooks().Measure(pids)
	if err != nil {
		log.Printf("apply: %q: can't measure the copies to see which a new max-memory kills: %v", spec.AppName, err)
		return nil
	}

	var heavy []heavyCopy
	for _, rec := range running {
		if size, ok := sizes[rec.PID]; ok && size > limit {
			heavy = append(heavy, heavyCopy{rec.Copy, killKeyOf(rec)})
		}
	}
	return heavy
}

// killClause is the end of apply's reply that names the copies the new limit is
// going to kill, or "" when there are none. A copy counts only if its record
// still names the process that was measured, since the cycle that just ran may
// have replaced it. The caller holds the state lock.
func killClause(spec manifest.AppSpec, heavy []heavyCopy) string {
	if len(heavy) == 0 {
		return ""
	}
	st, err := state.Load(spec.AppStateFile)
	if err != nil {
		return ""
	}
	var copies []int
	for _, h := range heavy {
		if rec, ok := st.Copy(h.copy); ok && killKeyOf(rec) == h.key {
			copies = append(copies, h.copy)
		}
	}
	if len(copies) == 0 {
		return ""
	}
	return fmt.Sprintf("; %s already %s more than %s and will be killed", reconcile.ListCopies(copies), plural(copies, "holds", "hold"), formatMem(spec.EffectiveMaxMemory()))
}
