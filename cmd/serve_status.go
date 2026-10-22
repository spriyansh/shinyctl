/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"net/http"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

// handleStatus reports on every app appRegistry knows about — every spec
// ever applied to this user's shinyctl-api and not since forgotten, across
// restarts, since the registry persists to ~/.shinyctl/registry.json.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := api.StatusResponse{Apps: []api.AppStatus{}}
	var alivePIDs []int

	for _, spec := range appRegistry.All() {
		currentState, err := state.Load(spec.AppStateFile)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		app := currentState.App()

		alive := process.IsAlive(spec, app)

		var ready bool
		if alive {
			ready = process.IsReady(spec.EffectiveHost(), app.Port)
		}

		if alive {
			alivePIDs = append(alivePIDs, app.PID)
		}

		resp.Apps = append(resp.Apps, api.AppStatus{
			// Name/Host/Port come from the desired spec, not the recorded
			// state: after a stop, state.json is cleared, so app.Name/
			// app.Port would otherwise render as a blank row instead of
			// identifying which app this is.
			Name:       spec.AppName,
			Host:       spec.EffectiveHost(),
			Port:       spec.Port,
			PID:        app.PID,
			StartedAt:  app.StartedAt,
			Alive:      alive,
			Ready:      ready,
			Stopped:    appRegistry.IsStopped(spec.AppStateFile),
			CrashCount: app.CrashCount,
		})
	}

	// Only processes that passed IsAlive are measured, so a recorded PID
	// that now belongs to something else never shows that process's memory.
	// One ps call for all of them.
	rss := process.RSS(alivePIDs)
	for i := range resp.Apps {
		if resp.Apps[i].Alive {
			resp.Apps[i].RSSBytes = rss[resp.Apps[i].PID]
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
