package api

import (
	"time"

	"github.com/spriyansh/shinyctl/internal/reconcile"
)

// ApplyResponse is shinyctl-api's response to the spec-carrying POSTs —
// /apply, /stop, /delete and /restart: which action it took, and a line for the
// CLI to print.
type ApplyResponse struct {
	Action  reconcile.ActionType `json:"action"`
	Message string               `json:"message"`
}

// AppStatus is one app's current, observed status — what state.json last
// recorded, plus live IsAlive/IsReady checks, not just a mirror of the
// file. Ready is purely informational: it reflects whether the app is
// actually accepting connections yet, but never drives a restart
// decision — only Alive does that (see reconcile.Decide).
type AppStatus struct {
	Name       string    `json:"name"`
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started-at"`
	Alive      bool      `json:"alive"`
	Ready      bool      `json:"ready"`
	Stopped    bool      `json:"stopped"`
	CrashCount int       `json:"crash-count"`

	// RSSBytes is the resident memory of the app's main process; zero when
	// it isn't running or couldn't be measured. A daemon from before this
	// field existed omits it, which reads the same.
	RSSBytes int64 `json:"rss-bytes,omitempty"`
}

// StatusResponse is shinyctl-api's response to a GET /status request:
// every app in its registry — applied at some point and not deleted
// since, across shinyctl-api's own restarts.
type StatusResponse struct {
	Apps []AppStatus `json:"apps"`
}
