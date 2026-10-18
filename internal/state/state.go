package state

import "time"

// AppState is what shinyctl-api last recorded about one app's process:
// identity (PID, name, port, entrypoint, when it was started) plus the
// crash history the reconcile loop's backoff and restart-policy decisions
// are made from.
type AppState struct {
	Name           string    `json:"name"`
	PID            int       `json:"pid"`
	Port           int       `json:"port"`
	EntrypointFile string    `json:"entrypoint-file"`
	StartedAt      time.Time `json:"started-at"`
	CrashCount     int       `json:"crash-count"`
	LastExitAt     time.Time `json:"last-exit-at"`
	LastExitFailed bool      `json:"last-exit-failed"`
}

// State is the on-disk shape of one app's state.json.
type State struct {
	AppGroup string     `json:"app-group"`
	Apps     []AppState `json:"apps"`
}

// App returns the one app this state records, or a zero AppState if
// nothing has been recorded yet (fresh file, or cleared by `stop`). The
// on-disk shape is a list for historical reasons, but shinyctl runs
// exactly one instance per app, so every reader only ever wants Apps[0].
func (s State) App() AppState {
	if len(s.Apps) > 0 {
		return s.Apps[0]
	}
	return AppState{}
}
