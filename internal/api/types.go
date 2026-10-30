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

// CopyStatus is one copy of an app: what state.json recorded about its
// process, plus live IsAlive/IsReady checks, not just a mirror of the file.
// Ready is purely informational (it never drives a restart decision, only
// Alive does): the copy's process accepts connections, and the load balancer
// serves the app's port and lists the copy (for a process from before the
// load balancer, whether its own port accepts).
type CopyStatus struct {
	Copy       int       `json:"copy"`
	Role       string    `json:"role,omitempty"` // "active", "warm", "draining", or empty when the load balancer doesn't list it
	PID        int       `json:"pid"`
	StartedAt  time.Time `json:"started-at"`
	Alive      bool      `json:"alive"`
	Ready      bool      `json:"ready"`
	CrashCount int       `json:"crash-count"`

	// GaveUp says the copy is not running and the restart policy brings it
	// back no more (until the next apply or restart).
	GaveUp bool `json:"gave-up,omitempty"`

	// Outdated says the copy runs a previous version, draining or not.
	Outdated bool `json:"outdated,omitempty"`

	// DrainEndsAt is when drain-timeout ends the copy's drain at the latest;
	// zero when it isn't draining.
	DrainEndsAt time.Time `json:"drain-ends-at,omitzero"`

	// SkipDrain says the copy stops without draining once it is replaced
	// (--now).
	SkipDrain bool `json:"skip-drain,omitempty"`

	// MemoryBytes is the memory of the copy's main process; zero when it
	// isn't running or couldn't be measured. The wire name is historical: it
	// was once the resident size, and is kept because the CLI and
	// shinyctl-api of different builds are common between replacing the
	// binary and restarting shinyctl-api.
	MemoryBytes int64 `json:"rss-bytes,omitempty"`

	// Sessions is the number of requests open to the copy through the load
	// balancer (a Shiny session is one open WebSocket); nil when unknown.
	Sessions *int `json:"sessions,omitempty"`
}

// AppStatus is one app's current, observed status, with a row per copy.
type AppStatus struct {
	Name         string       `json:"name"`
	Host         string       `json:"host"`
	Port         int          `json:"port"`
	Stopped      bool         `json:"stopped"`
	Replicas     int          `json:"replicas"`
	WarmReplicas int          `json:"warm-replicas"`
	Copies       []CopyStatus `json:"copies"`

	// RestartPolicy is the app's restart-policy as it applies (always,
	// on-failure or never), so a copy the policy leaves down can be explained.
	RestartPolicy string `json:"restart-policy,omitempty"`

	// NotServed is why the load balancer leaves the app's port unserved (it
	// is taken by another program or another app), or empty. The copies may
	// be running fine behind it.
	NotServed string `json:"not-served,omitempty"`

	// NotStarted is why the app's copies aren't started: its port is taken
	// by another program. It is not a crash and is not counted as one.
	NotStarted string `json:"not-started,omitempty"`
}

// StatusResponse is shinyctl-api's response to a GET /status request:
// every app in its registry — applied at some point and not deleted
// since, across shinyctl-api's own restarts.
type StatusResponse struct {
	Apps []AppStatus `json:"apps"`

	// BalancerMemory is the memory of the load balancer's process; zero when
	// it couldn't be measured. The wire name is historical, as for
	// CopyStatus.MemoryBytes.
	BalancerMemory int64 `json:"balancer-rss-bytes,omitempty"`

	// BalancerStarting says the load balancer is being started or replaced by
	// one of another build, and does not answer for the moment.
	BalancerStarting bool `json:"balancer-starting,omitempty"`
}
