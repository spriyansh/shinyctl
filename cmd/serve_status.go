/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// handleStatus reports on every app appRegistry knows about — every spec
// ever applied to this user's shinyctl-api and not since forgotten, across
// restarts, since the registry persists to ~/.shinyctl/registry.json — with a
// row per copy.
func handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := api.StatusResponse{Apps: []api.AppStatus{}}
	var alivePIDs []int

	// One question to the load balancer for every app, and none that waits
	// for a reload in progress.
	sessions := gateway.Sessions()

	// The registry keeps its apps in a map: a fixed order is what makes two
	// calls to status comparable.
	specs := appRegistry.All()
	slices.SortFunc(specs, func(a, b manifest.AppSpec) int {
		return cmp.Or(strings.Compare(a.AppName, b.AppName), strings.Compare(a.AppGroup, b.AppGroup), cmp.Compare(a.Port, b.Port), strings.Compare(a.AppStateFile, b.AppStateFile))
	})

	for _, spec := range specs {
		currentState, err := state.Load(spec.AppStateFile)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		recs := currentState.Copies()
		alive := process.AliveAll(spec, recs)
		pool, _ := gateway.Pool(spec.AppStateFile)
		serves := gateway.Serves(spec.AppStateFile, spec.EffectiveHost(), spec.Port)

		app := api.AppStatus{
			// Name/Host/Port come from the desired spec, not the recorded
			// state: after a stop, state.json is cleared, so a record's name
			// and port would otherwise render as a blank row identifying
			// nothing.
			Name:          spec.AppName,
			Host:          spec.EffectiveHost(),
			Port:          spec.Port,
			Stopped:       appRegistry.IsStopped(spec.AppStateFile),
			Replicas:      spec.EffectiveReplicas(),
			WarmReplicas:  spec.EffectiveWarm(),
			Copies:        []api.CopyStatus{},
			RestartPolicy: string(spec.RestartPolicy.Effective()),
			NotServed:     gateway.NotServed(spec.AppStateFile),
			NotStarted:    notStartedReason(spec.AppStateFile),
		}

		for i, rec := range recs {
			dial := dialOf(rec)
			copy := api.CopyStatus{
				Copy:       rec.Copy,
				PID:        rec.PID,
				StartedAt:  rec.StartedAt,
				Alive:      alive[i],
				CrashCount: rec.CrashCount,
			}

			switch {
			case slices.Contains(pool.Active, dial):
				copy.Role = "active"
			case slices.Contains(pool.Warm, dial):
				copy.Role = "warm"
			case slices.Contains(pool.Draining, dial):
				copy.Role = "draining"
			}

			if alive[i] {
				alivePIDs = append(alivePIDs, rec.PID)

				// Ready needs the process answering and, for a process behind
				// the load balancer, the load balancer serving the app's own
				// port and listing the copy; the public port is not probed,
				// since the load balancer accepts there even while the
				// process is down. A record from before the load balancer
				// still holds the app's port itself.
				if rec.Copy >= 1 {
					copy.Ready = process.IsReady(process.InternalHost, rec.Port) && copy.Role != "" && serves
					if sessions != nil {
						n := sessions[dial]
						copy.Sessions = &n
					}
				} else {
					copy.Ready = process.IsReady(spec.EffectiveHost(), rec.Port)
				}
			}
			copy.SkipDrain = rec.SkipDrain
			if alive[i] && !rec.DrainingSince.IsZero() {
				copy.DrainEndsAt = rec.DrainingSince.Add(spec.EffectiveDrainTimeout())
			}
			copy.GaveUp = !alive[i] && reconcile.GaveUp(spec, rec)
			changed, restart := reconcile.Outdated(spec, currentState.Revision, rec)
			copy.Outdated = alive[i] && (len(changed) > 0 || restart)

			app.Copies = append(app.Copies, copy)
		}
		resp.Apps = append(resp.Apps, app)
	}

	// Only processes that passed the identity check are measured, so a
	// recorded PID that now belongs to something else never shows that
	// process's memory. One ps call for all of them and the load balancer.
	balancer := gateway.BalancerPID()
	if balancer > 0 {
		alivePIDs = append(alivePIDs, balancer)
	}
	rss := process.RSS(alivePIDs)
	for i := range resp.Apps {
		for j := range resp.Apps[i].Copies {
			if c := &resp.Apps[i].Copies[j]; c.Alive {
				c.RSSBytes = rss[c.PID]
			}
		}
	}
	resp.BalancerRSS = rss[balancer]

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
