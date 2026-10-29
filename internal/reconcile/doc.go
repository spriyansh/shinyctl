// Package reconcile holds the pure decisions of the reconcile cycle: what each
// copy of an app needs (DecideCopies), whether a copy runs an older version
// than the manifest (Outdated), what a crash costs (RecordExit, the backoff,
// GaveUp), and the wording of what the cycle did. It reads and writes nothing
// and starts nothing: the cycle in package cmd observes the machine, calls
// these functions with what it saw, and carries out the plan.
package reconcile
