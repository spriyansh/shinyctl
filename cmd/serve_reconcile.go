/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"context"
	"log"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/registry"
)

// appRegistry is shared between handleApply (an HTTP handler goroutine per
// request) and runReconcileLoop (one long-lived ticker goroutine) — both
// touch the same specs, which is exactly the Registry's job to make safe.
// `serve` replaces this with the disk-backed registry.Open before either
// of those starts; the in-memory default is what tests run against.
var appRegistry = registry.New()

const reconcileInterval = 5 * time.Second

// runReconcileLoop re-runs reconcileSpec for every remembered app on a
// fixed tick, until ctx is cancelled. This is what makes a killed app
// come back on its own, instead of only on the next /apply request.
func runReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileOnce()
		}
	}
}

func reconcileOnce() {
	for _, spec := range appRegistry.All() {
		triggerReconcile(spec)
		rotateAppLog(spec)
	}
	rotateAPILog()
}

// rotateAppLog turns spec's log over if it's past process.MaxLogSize.
// Piggybacks on the reconcile tick because that's the one place every
// remembered app is already visited periodically; a stat per app per 5s
// is nothing. Errors are logged, never fatal — a log that can't be
// rotated is a nuisance, an app that isn't reconciled is an outage.
func rotateAppLog(spec manifest.AppSpec) {
	logPath, err := process.AppLogPath(spec)
	if err != nil {
		log.Printf("rotate: %q: %v", spec.AppName, err)
		return
	}
	if rotated, err := process.RotateLog(logPath, process.MaxLogSize); err != nil {
		log.Printf("rotate: %q: %v", spec.AppName, err)
	} else if rotated {
		log.Printf("rotate: %q: log exceeded %d bytes, rotated to %s.1", spec.AppName, process.MaxLogSize, logPath)
	}
}

// rotateAPILog is rotateAppLog for shinyctl-api's own log. Works the same
// way for the same reason: this process writes api.log through the
// O_APPEND descriptor LaunchDetached handed it as stdout, so truncation
// underneath it is safe.
func rotateAPILog() {
	logPath, err := process.LogPath()
	if err != nil {
		log.Printf("rotate: api.log: %v", err)
		return
	}
	if rotated, err := process.RotateLog(logPath, process.MaxLogSize); err != nil {
		log.Printf("rotate: api.log: %v", err)
	} else if rotated {
		log.Printf("rotate: api.log exceeded %d bytes, rotated to %s.1", process.MaxLogSize, logPath)
	}
}

// triggerReconcile runs one reconcileSpec cycle for spec and logs the
// outcome if it started something or backed off. Called both by the ticker, on its
// fixed interval, and directly the instant a managed process is detected
// to have exited (see reconcileSpec's exit-watcher in cmd/reconcile.go) —
// the ticker is the safety net, the direct call is what makes restart
// near-instant for apps shinyctl-api itself started this run.
//
// Checking appRegistry.Managed here, rather than inside reconcileSpec or
// Decide, is deliberate: it's what lets handleStop and handleDelete kill a
// process without the already-running exit-watcher goroutine (which calls
// this same function, not reconcileSpec directly) immediately restarting
// it — stopped and forgotten apps are both unmanaged. handleApply calls
// reconcileSpec directly, bypassing this check entirely — applying always
// means "ensure this is running now."
func triggerReconcile(spec manifest.AppSpec) {
	if !appRegistry.Managed(spec.AppStateFile) {
		return
	}

	resp, err := reconcileSpec(spec, false)
	if err != nil {
		log.Printf("reconcile: %q: %v", spec.AppName, err)
		return
	}

	if resp.Action == reconcile.ActionStart || resp.Action == reconcile.ActionBackoff {
		log.Printf("reconcile: %s", resp.Message)
	}
}
