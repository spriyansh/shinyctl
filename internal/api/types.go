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

	// MaxMemoryBytes is the max-memory limit the daemon applied, 0 for none.
	// A daemon from before the limit leaves it out, which is how the CLI tells
	// that it ignores a limit it was asked to apply.
	MaxMemoryBytes int64 `json:"max-memory-bytes,omitempty"`

	// MachineMemory is the machine's memory when it is low, for a warning
	// that the copies just started need memory too; nil otherwise.
	MachineMemory *MachineMemory `json:"machine-memory,omitempty"`
}

// ShutdownResponse is shinyctl-api's answer to /shutdown, sent before it exits.
// Errors do not stop a shutdown (its loops are cancelled and it cannot carry
// on), so they come back here as Problems next to what was stopped.
type ShutdownResponse struct {
	PID int `json:"pid"` // the shinyctl-api process that is exiting

	// Apps are the apps with at least one copy that was stopped, by name.
	Apps []string `json:"apps,omitempty"`

	// Balancer says whether a load balancer was stopped.
	Balancer bool `json:"balancer,omitempty"`

	// Notes are things to tell the user that are not problems, such as a
	// recorded process that was left alone because it is not the app's.
	Notes []string `json:"notes,omitempty"`

	// Problems are what could not be stopped. Their records are kept, so
	// running shutdown again retries.
	Problems []string `json:"problems,omitempty"`
}

// CopyStatus is one copy of an app: what state.json recorded about its
// process, plus live IsAlive/Listening checks, not just a mirror of the file.
// Ready means the copy's process accepts connections, its page has answered,
// and the load balancer serves the app's port and lists the copy (for a
// process from before the load balancer, whether its own port accepts).
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

	// LastExitReason is why shinyctl ended the copy's last process, "oom" for
	// one killed for going over max-memory; empty for an exit nobody caused.
	LastExitReason string `json:"last-exit-reason,omitempty"`

	// MemoryBytes is the memory of the copy's processes, the main one and
	// the workers it started; zero when it isn't running or couldn't be
	// measured. The wire name is historical: it
	// was once the resident size, and is kept because the CLI and
	// shinyctl-api of different builds are common between replacing the
	// binary and restarting shinyctl-api.
	MemoryBytes int64 `json:"rss-bytes,omitempty"`

	// Sessions is the number of requests open to the copy through the load
	// balancer (a Shiny session is one open WebSocket); nil when unknown.
	Sessions *int `json:"sessions,omitempty"`

	// PageProblem is what the copy's page answered instead of the page ("answers
	// HTTP 500", read after "its page") while that keeps the copy from being
	// ready; empty once the page has answered, or before any answer.
	PageProblem string `json:"page-problem,omitempty"`

	// PageWaitingSince is when the request for the copy's page that is still
	// unanswered was sent, once it has waited process.PageSlow; zero otherwise.
	PageWaitingSince time.Time `json:"page-waiting-since,omitzero"`
}

// AppStatus is one app's current, observed status, with a row per copy.
type AppStatus struct {
	Name string `json:"name"`

	// Group is the app's app-group, which tells two apps of the same name
	// apart. An older shinyctl-api does not send it.
	Group string `json:"group,omitempty"`

	Host         string       `json:"host"`
	Port         int          `json:"port"`
	Stopped      bool         `json:"stopped"`
	Replicas     int          `json:"replicas"`
	WarmReplicas int          `json:"warm-replicas"`
	Copies       []CopyStatus `json:"copies"`

	// RestartPolicy is the app's restart-policy as it applies (always,
	// on-failure or never), so a copy the policy leaves down can be explained.
	RestartPolicy string `json:"restart-policy,omitempty"`

	// MaxMemoryBytes is the app's max-memory in bytes; 0 for no limit.
	MaxMemoryBytes int64 `json:"max-memory-bytes,omitempty"`

	// NotServed is why the load balancer leaves the app's port unserved (it
	// is taken by another program or another app), or empty. The copies may
	// be running fine behind it.
	NotServed string `json:"not-served,omitempty"`

	// NotStarted is why the app's copies aren't started: its port is taken
	// by another program. It is not a crash and is not counted as one.
	NotStarted string `json:"not-started,omitempty"`
}

// MachineMemory is the memory of the whole machine as shinyctl-api last read
// it, for the note that it is low. Pressure is set on macOS only; there,
// AvailableBytes is derived from the kernel's free percentage, and the swap
// total is the size of the swap files, which macOS adds as needed.
type MachineMemory struct {
	Low            bool  `json:"low,omitempty"`
	TotalBytes     int64 `json:"total-bytes"`
	AvailableBytes int64 `json:"available-bytes"`
	SwapTotalBytes int64 `json:"swap-total-bytes,omitempty"`
	SwapUsedBytes  int64 `json:"swap-used-bytes,omitempty"`

	// StallPercent is how much of the last minute a process was waiting
	// for memory, from the kernel's pressure stall information, as a whole
	// percent, truncated; 0 under one percent and where the kernel does
	// not provide it. Linux only.
	StallPercent int `json:"stall-percent,omitempty"`

	// Pressure is the kernel's level on macOS: normal, warning or critical,
	// or unknown for a level this build does not know.
	Pressure string `json:"pressure,omitempty"`
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

	// MachineMemory is the memory of the whole machine, healthy or low; nil
	// when shinyctl-api has no reading, or is an older build.
	MachineMemory *MachineMemory `json:"machine-memory,omitempty"`
}
