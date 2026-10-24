package state

import (
	"slices"
	"time"
)

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

	// Revision is State.Revision when this copy was started. A copy whose
	// revision is below the state's runs code from before the last restart.
	// Absent means 0.
	Revision int `json:"revision,omitempty"`
}

// State is the on-disk shape of one app's state.json.
//
// Apps holds one record per copy of the app, sorted by copy number. A
// phase-1 file (one record, copy 1, no revision) reads as copy 1 at
// revision 0 under a state at revision 0, so nothing needs migrating. A
// record with copy 0 is from before the load balancer, and is only ever the
// sole record.
//
// This key is reserved for rolling out new versions, and nothing writes it
// yet. A record that lacks it reads as a copy that is not draining:
//   - AppState.draining-since (time): absent means the copy is not draining.
type State struct {
	AppGroup string `json:"app-group"`

	// Revision is raised by restart. Absent means 0. A copy that was started
	// at a lower revision is outdated.
	Revision int `json:"revision,omitempty"`

	Apps []AppState `json:"apps"`
}

// App returns the one app this state records, or a zero AppState if
// nothing has been recorded yet (fresh file, or cleared by `stop`).
func (s State) App() AppState {
	if len(s.Apps) > 0 {
		return s.Apps[0]
	}
	return AppState{}
}

// Copies returns the records sorted by copy number, as a new slice.
func (s State) Copies() []AppState {
	out := slices.Clone(s.Apps)
	slices.SortStableFunc(out, func(a, b AppState) int { return a.Copy - b.Copy })
	return out
}

// Copy returns the record of copy n.
func (s State) Copy(n int) (AppState, bool) {
	for _, a := range s.Apps {
		if a.Copy == n {
			return a, true
		}
	}
	return AppState{}, false
}

// Put replaces the record with a's copy number, or adds it, keeping the
// records sorted.
func (s *State) Put(a AppState) {
	if i := slices.IndexFunc(s.Apps, func(r AppState) bool { return r.Copy == a.Copy }); i >= 0 {
		s.Apps[i] = a
	} else {
		s.Apps = append(s.Apps, a)
	}
	s.Apps = s.Copies()
}

// Remove drops the record of copy n, if there is one.
func (s *State) Remove(n int) {
	s.Apps = slices.DeleteFunc(s.Apps, func(r AppState) bool { return r.Copy == n })
}
