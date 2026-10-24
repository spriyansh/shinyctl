package state

import "time"

// AppState is what shinyctl-api last recorded about one app's process:
// identity (PID, name, port, entrypoint, when it was started), the rest of
// the manifest it was started from, plus the crash history the reconcile
// loop's backoff and restart-policy decisions are made from.
type AppState struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`

	// Port is the port the process listens on. For a copy (Copy >= 1) it is
	// an internal port on 127.0.0.1, behind the load balancer that holds the
	// app's own port. A record without a copy number was written by a build
	// before the load balancer, and its process holds the app's port itself
	// (see reconcile.Decide's Upgrade).
	Port int `json:"port"`

	// Copy numbers the app's processes from 1. Zero marks a record from
	// before the load balancer.
	Copy           int       `json:"copy,omitempty"`
	EntrypointFile string    `json:"entrypoint-file"`
	StartedAt      time.Time `json:"started-at"`
	CrashCount     int       `json:"crash-count"`
	LastExitAt     time.Time `json:"last-exit-at"`
	LastExitFailed bool      `json:"last-exit-failed"`

	// StartedFrom is manifest.AppSpec.StartFields() at the time the process
	// was started: the settings that shaped it beyond the typed fields
	// above. A key that is absent means unknown, not changed (records from
	// before the key existed have no such key), so it never triggers a
	// restart.
	StartedFrom map[string]string `json:"started-from,omitempty"`
}

// State is the on-disk shape of one app's state.json.
//
// Apps holds one entry per copy of the app; for now there is exactly one,
// copy 1. These keys are reserved for running several copies, and nothing
// writes them yet. A record that lacks them reads as the oldest, only copy,
// so adding them needs no migration:
//   - State.revision (int): absent means 0; a restart will raise it.
//   - AppState.revision (int): absent means 0; a copy is outdated if its
//     revision is below State.revision.
//   - AppState.draining-since (time): absent means the copy is not draining.
type State struct {
	AppGroup string     `json:"app-group"`
	Apps     []AppState `json:"apps"`
}

// App returns the one app this state records, or a zero AppState if
// nothing has been recorded yet (fresh file, or cleared by `stop`). The
// on-disk shape is a list because an app will have several copies; for now
// shinyctl runs exactly one, so every reader only ever wants Apps[0].
func (s State) App() AppState {
	if len(s.Apps) > 0 {
		return s.Apps[0]
	}
	return AppState{}
}
