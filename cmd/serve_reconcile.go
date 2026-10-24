/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/registry"
	"github.com/spriyansh/shinyctl/internal/state"
)

// appRegistry is shared between handleApply (an HTTP handler goroutine per
// request) and runReconcileLoop (one long-lived ticker goroutine) — both
// touch the same specs, which is exactly the Registry's job to make safe.
// `serve` replaces this with the disk-backed registry.Open before either
// of those starts; the in-memory default is what tests run against.
var appRegistry = registry.New()

const reconcileInterval = 5 * time.Second

// runReconcileLoop reconciles every remembered app on a fixed tick, until
// ctx is cancelled. This is what makes a killed app come back on its own,
// instead of only on the next /apply request.
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
		// Right after each start, not once at the end of the tick: an app
		// moving behind the load balancer has no listener on its port
		// until the sync, and the apps after it in this tick may each take
		// several seconds to stop their old process. The gateway logs what
		// it can't do, so the error isn't repeated here.
		if reconcileApp(spec.AppStateFile) {
			syncProxy("")
		}
		rotateAppLog(spec)
	}
	syncProxy("")
	rotateAPILog()
}

// rotateAppLog turns every one of spec's copies' logs over if it's past
// process.MaxLogSize. Piggybacks on the reconcile tick because that's the
// one place every remembered app is already visited periodically; a stat per
// log per 5s is nothing. Errors are logged, never fatal — a log that can't be
// rotated is a nuisance, an app that isn't reconciled is an outage.
func rotateAppLog(spec manifest.AppSpec) {
	logs, err := process.AppLogPaths(spec)
	if err != nil {
		log.Printf("rotate: %q: %v", spec.AppName, err)
		return
	}
	for _, l := range logs {
		if rotated, err := process.RotateLog(l.Path, process.MaxLogSize); err != nil {
			log.Printf("rotate: %q: %v", spec.AppName, err)
		} else if rotated {
			log.Printf("rotate: %q: log exceeded %d bytes, rotated to %s.1", spec.AppName, process.MaxLogSize, l.Path)
		}
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

// triggerReconcile is reconcileApp followed by a sync of the load balancer,
// which is what a process exit needs: an app that has given up loses its
// port. Called by an exit-watcher the instant a process dies.
func triggerReconcile(stateFile string) {
	reconcileApp(stateFile)
	syncProxy("")
}

// reconcileApp runs one reconcile cycle for the app at stateFile and
// logs the outcome if it started something or backed off. It reports
// whether it took ActionStart. Called both by the ticker, on its fixed
// interval, and (through triggerReconcile) directly the instant a managed
// process is detected to have exited (see launchCopy's exit-watcher in
// cmd/reconcile.go) — the ticker is the safety net, the direct call is
// what makes restart near-instant for apps shinyctl-api itself started
// this run.
//
// Checking appRegistry.Managed here, rather than inside reconcileLocked or
// DecideCopies, is deliberate: it's what lets handleStop and handleDelete kill a
// process without the already-running exit-watcher goroutine (which calls
// this function, not reconcileLocked directly) immediately restarting
// it — stopped and forgotten apps are both unmanaged. handleApply calls
// reconcileLocked directly, bypassing this check entirely — applying always
// means "ensure this is running now." handleRestart likewise calls
// reconcileLocked directly. Both do it under their own hold of the state
// lock.
//
// It takes the state key, not a spec, and reads the spec under the state
// lock: a spec captured earlier (by an exit-watcher, or by the ticker's
// snapshot) may predate a re-apply, and reconciling with it would undo the
// replacement.
func reconcileApp(stateFile string) bool {
	// Cheap gate before the lock, so this never queues behind a stop or
	// delete that is holding it.
	if !appRegistry.Managed(stateFile) {
		return false
	}

	unlock := state.Lock(stateFile)
	defer unlock()

	spec, ok := appRegistry.ManagedSpec(stateFile)
	if !ok {
		return false
	}

	resp, err := reconcileLocked(spec, reconcileOpts{})
	if err != nil {
		// A port that is taken has been logged when it first was, and is
		// tried again every tick without saying so again.
		if !errors.As(err, new(notStartedError)) {
			log.Printf("reconcile: %q: %v", spec.AppName, err)
		}
		return false
	}

	if resp.Action == reconcile.ActionStart || resp.Action == reconcile.ActionBackoff {
		log.Printf("reconcile: %s", resp.Message)
	}
	return resp.Action == reconcile.ActionStart
}
