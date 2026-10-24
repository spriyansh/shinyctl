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

		// Ready needs the process answering and, for a process behind the
		// load balancer, the load balancer serving the app's own port; the
		// public port is not probed, since the load balancer accepts there
		// even while the process is down. A record from before the load
		// balancer still holds the app's port itself.
		var ready bool
		if alive {
			if app.Copy >= 1 {
				ready = process.IsReady(process.InternalHost, app.Port) &&
					gateway.Serves(spec.AppStateFile, spec.EffectiveHost(), spec.Port)
			} else {
				ready = process.IsReady(spec.EffectiveHost(), app.Port)
			}
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
			NotServed:  gateway.NotServed(spec.AppStateFile),
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
