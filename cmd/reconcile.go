/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"
	"log"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// reconcileSpec runs one decide-and-act cycle for spec: load state, decide,
// and on ActionStart, launch the process and persist the new state. Shared
// between the HTTP /apply handler (triggered by a client request) and the
// background reconcile loop (triggered by a ticker or an exit-watcher
// goroutine) — they differ only in what causes this to run, and in
// resetBackoff: an explicit apply always means "ensure this is running
// now," so it clears any crash-loop history; a tick or an exit does not.
func reconcileSpec(spec manifest.AppSpec, resetBackoff bool) (api.ApplyResponse, error) {
	// Held across the whole cycle, through process.Start: an apply and a
	// tick deciding ActionStart off the same loaded state would otherwise
	// both launch, and the loser's PortAvailable failure gets recorded as
	// a phantom crash.
	unlock := state.Lock(spec.AppStateFile)
	defer unlock()

	currentState, err := state.Load(spec.AppStateFile)
	if err != nil {
		return api.ApplyResponse{}, err
	}
	app := currentState.App()

	// Clear the whole exit record, not just CrashCount: Decide gates the
	// restart policy on LastExitAt, so leaving it set would let a `never`
	// app stay given-up through an explicit apply.
	if resetBackoff {
		app.CrashCount = 0
		app.LastExitAt = time.Time{}
		app.LastExitFailed = false
	}

	action := reconcile.Decide(spec, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{app}}, process.IsAlive(spec, app), time.Now())
	resp := api.ApplyResponse{Action: action.Type}

	switch action.Type {
	case reconcile.ActionNone:
		resp.Message = "Nothing to change"

	case reconcile.ActionBackoff:
		resp.Message = fmt.Sprintf("Backing off restart of %q after %d crash(es)", spec.AppName, app.CrashCount)

	case reconcile.ActionStart:
		// Checked here, not in manifest.Validate: by this point Decide has
		// already confirmed our own last-recorded instance (if any) isn't
		// alive, so a port that's occupied right now genuinely belongs to
		// something else — Validate has no state to make that distinction
		// and would wrongly reject every routine re-apply of an app that's
		// correctly, already running on its own port.
		if err := process.PortAvailable(spec.EffectiveHost(), spec.Port); err != nil {
			recordExitLocked(spec, process.Exit{At: time.Now(), Failed: true})
			return api.ApplyResponse{}, err
		}

		newApp, done, err := process.Start(spec)
		if err != nil {
			// A failed exec() is a crash too — it just never produces an
			// exit-channel event, since there was never a process to
			// Wait() on. Without this, a bad entrypoint/missing Rscript
			// would retry every tick forever with no backoff, exactly
			// like the bug this whole mechanism exists to prevent.
			recordExitLocked(spec, process.Exit{At: time.Now(), Failed: true})
			return api.ApplyResponse{}, err
		}

		// Carry crash history forward across the restart itself — it's
		// only RecordExit, below, that ever changes it. Without this a
		// successful restart would silently wipe the crash count the
		// backoff logic above just relied on.
		newApp.CrashCount = app.CrashCount
		newApp.LastExitAt = app.LastExitAt
		newApp.LastExitFailed = app.LastExitFailed

		// React to this specific process dying immediately, instead of
		// waiting for the next reconcile tick (up to reconcileInterval
		// later). Safe even if a newer process has already replaced this
		// one by the time it fires: triggerReconcile re-reads state.json
		// fresh and re-derives from there, it doesn't trust anything
		// captured in this closure except which spec to check.
		go func() {
			exit := <-done
			recordExit(spec, exit)
			triggerReconcile(spec)
		}()

		apps := state.State{
			AppGroup: spec.AppGroup,
			Apps:     []state.AppState{newApp},
		}

		if err := state.Save(spec.AppStateFile, apps); err != nil {
			return api.ApplyResponse{}, err
		}

		resp.Message = fmt.Sprintf("Starting app %q on port %d", spec.AppName, spec.Port)

	default:
		return api.ApplyResponse{}, fmt.Errorf("unhandled action type %q", action.Type)
	}

	return resp, nil
}

// recordExit persists exit into spec's state.json ahead of the
// triggerReconcile call that follows it, so Decide sees up-to-date crash
// history on the very next reconcile pass. Kept as its own disk
// read-modify-write rather than folded into reconcileSpec's, since it has
// to run unconditionally on every exit, independent of whatever action
// that next reconcile pass decides on. Takes the state lock itself — for
// the exit-watcher goroutine, which holds nothing.
//
// An exit caused by `stop` or `delete` is not a crash: both mark the app
// unmanaged before signaling it, so by the time this runs for that exit
// there's nothing to record — writing one would show as a phantom
// `CRASHES 1` in status, or (for delete) recreate the state file that was
// just removed.
func recordExit(spec manifest.AppSpec, exit process.Exit) {
	if !appRegistry.Managed(spec.AppStateFile) {
		return
	}

	unlock := state.Lock(spec.AppStateFile)
	defer unlock()
	recordExitLocked(spec, exit)
}

// recordExitLocked is recordExit for a caller that already holds the
// state lock — reconcileSpec, recording a Start failure mid-cycle.
func recordExitLocked(spec manifest.AppSpec, exit process.Exit) {
	currentState, err := state.Load(spec.AppStateFile)
	if err != nil {
		log.Printf("reconcile: %q: recording exit: %v", spec.AppName, err)
		return
	}

	app := reconcile.RecordExit(currentState.App(), exit)

	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{app}}); err != nil {
		log.Printf("reconcile: %q: recording exit: %v", spec.AppName, err)
	}
}
