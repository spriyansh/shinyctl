package reconcile

import "github.com/spriyansh/shinyctl/internal/manifest"

// ActionType names what shinyctl-api did, or decided to do, for an app.
// Decide only ever produces None, Start and Backoff; Stop, Delete and Restart
// are reported by their handlers, which don't go through Decide.
type ActionType string

const (
	ActionNone    ActionType = "none"    // already as declared, or given up on (restart-policy)
	ActionStart   ActionType = "start"   // launch the process
	ActionStop    ActionType = "stop"    // stopped and parked until the next apply
	ActionDelete  ActionType = "delete"  // stopped and forgotten: gone from status, state file removed
	ActionRestart ActionType = "restart" // stopped if it was running, then started again from the current manifest
	ActionBackoff ActionType = "backoff" // crash-looping; waiting out a delay before the next restart attempt
)

// Action is Decide's answer: what to do, and for Start/Backoff, the spec
// to do it with.
type Action struct {
	Type ActionType
	Spec manifest.AppSpec

	// Changed is set on a Start that replaces a live process: the manifest
	// fields the process was started from no longer match, in fieldOrder.
	Changed []string

	// Upgrade is set on a Start that replaces a process from before the
	// load balancer: it holds the app's own port itself, so it is stopped,
	// the port claimed, and the app started again behind the load balancer.
	Upgrade bool
}
