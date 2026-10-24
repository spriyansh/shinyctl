package reconcile

// ActionType names what shinyctl-api did, or decided to do, for an app.
// DecideCopies' steps are only ever Start, Stop and Backoff, and a plan comes
// to None, Start or Backoff (see Plan.Action); Stop, Delete and Restart are
// also reported by their handlers.
type ActionType string

const (
	ActionNone    ActionType = "none"    // already as declared, or given up on (restart-policy)
	ActionStart   ActionType = "start"   // launch a process
	ActionStop    ActionType = "stop"    // stopped and parked until the next apply
	ActionDelete  ActionType = "delete"  // stopped and forgotten: gone from status, state file removed
	ActionRestart ActionType = "restart" // stopped if it was running, then started again from the current manifest
	ActionBackoff ActionType = "backoff" // crash-looping; waiting out a delay before the next restart attempt
)
