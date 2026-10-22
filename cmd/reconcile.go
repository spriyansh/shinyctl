/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// previousInstanceNote is the reply suffix for a start that first stopped
// app's process: the replacement path here, and restart. changed names the
// manifest fields behind it, if any.
func previousInstanceNote(app state.AppState, changed []string) string {
	if len(changed) == 0 {
		return fmt.Sprintf(" (stopped the previous instance: pid %d, port %d)", app.PID, app.Port)
	}
	return fmt.Sprintf(" (manifest changed: %s; stopped the previous instance: pid %d, port %d)", strings.Join(changed, ", "), app.PID, app.Port)
}

// reconcileLocked runs one decide-and-act cycle for spec: load state,
// decide, and on ActionStart stop the recorded process if it is still
// running from a manifest that no longer matches, then launch the
// replacement and persist the new state. The caller holds the state lock,
// held across the whole cycle, through process.Start: an apply and a tick
// deciding ActionStart off the same loaded state would otherwise both
// launch, and the loser's PortAvailable failure gets recorded as a phantom
// crash. The callers are handleApply, which takes the lock before it
// updates the registry; handleRestart, which holds it across its stop; and
// triggerReconcile, which takes it first so it can read the registry's
// current spec under it. The exit-watchers and the ticker reach it through
// triggerReconcile.
//
// resetBackoff: an explicit apply always means "ensure this is running
// now," so it clears any crash-loop history; a tick or an exit does not.
// settings names manifest fields that changed and take effect without a
// restart (see reconcile.SettingsChanged); they only appear in the reply.
func reconcileLocked(spec manifest.AppSpec, resetBackoff bool, settings []string) (api.ApplyResponse, error) {
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

	alive := process.IsAlive(spec, app)
	action := reconcile.Decide(spec, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{app}}, alive, time.Now())
	resp := api.ApplyResponse{Action: action.Type}

	switch action.Type {
	case reconcile.ActionNone:
		resp.Message = "Nothing to change"
		if len(settings) > 0 {
			resp.Message = fmt.Sprintf("Updated %q without a restart (manifest changed: %s)", spec.AppName, strings.Join(settings, ", "))
		}

	case reconcile.ActionBackoff:
		resp.Message = fmt.Sprintf("Backing off restart of %q after %d crash(es)", spec.AppName, app.CrashCount)

	case reconcile.ActionStart:
		// Decide only answers Start for a live recorded process when the
		// manifest it was started from has changed in a field that shapes
		// the process. Left running, that process is orphaned: it keeps
		// serving, but the Save below drops it from state.json, so status
		// can't see it and delete can't stop it. IsAlive matched its
		// recorded entrypoint, so it is ours to stop. This happens before
		// the port check because most changes keep the port, which the old
		// process still holds.
		var replaced string
		fields := reconcile.OrderFields(append(slices.Clone(action.Changed), settings...))
		if alive {
			log.Printf("reconcile: %q: manifest changed (%s): stopping the previous instance (%q, pid %d, port %d) to replace it", spec.AppName, strings.Join(fields, ", "), app.Name, app.PID, app.Port)
			// Stop re-checks identity before each signal, so a PID that
			// changed hands since the check above is left alone. A failed
			// Stop records no crash: nothing new ran.
			if _, err := process.Stop(app, spec.EntrypointFile); err != nil {
				return api.ApplyResponse{}, fmt.Errorf("stopping the previous instance of %q (pid %d): %w", spec.AppName, app.PID, err)
			}
			replaced = previousInstanceNote(app, fields)

			// Keep only the crash history. Any failure below is recorded
			// on this cleared record, and the stopped process's
			// exit-watcher sees a PID that isn't its own and stays quiet.
			app = state.AppState{CrashCount: app.CrashCount, LastExitAt: app.LastExitAt, LastExitFailed: app.LastExitFailed}
			if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{app}}); err != nil {
				return api.ApplyResponse{}, err
			}
		}

		// Checked here, not in manifest.Validate: by this point our own
		// last-recorded instance (if any) is either dead, per Decide, or
		// was just stopped above, so a port that's occupied right now
		// genuinely belongs to something else — Validate has no state to
		// make that distinction and would wrongly reject every routine
		// re-apply of an app that's correctly, already running on its own
		// port.
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
		// later). Only this PID's exit counts: if a replacement has since
		// taken over the record, recordExit refuses it and nothing is
		// reconciled. The reconcile itself uses the registry's current
		// spec, not the one captured here, which may predate a re-apply.
		pid := newApp.PID
		go func() {
			exit := <-done
			if recordExit(spec, pid, exit) {
				triggerReconcile(spec.AppStateFile)
			}
		}()

		apps := state.State{
			AppGroup: spec.AppGroup,
			Apps:     []state.AppState{newApp},
		}

		if err := state.Save(spec.AppStateFile, apps); err != nil {
			return api.ApplyResponse{}, err
		}

		if !alive && len(settings) > 0 {
			replaced = fmt.Sprintf(" (manifest changed: %s)", strings.Join(fields, ", "))
		}
		resp.Message = fmt.Sprintf("Starting app %q on port %d%s", spec.AppName, spec.Port, replaced)

	default:
		return api.ApplyResponse{}, fmt.Errorf("unhandled action type %q", action.Type)
	}

	return resp, nil
}

// recordExit persists exit into spec's state.json ahead of the
// triggerReconcile call that follows it, so Decide sees up-to-date crash
// history on the very next reconcile pass. Kept as its own disk
// read-modify-write rather than folded into reconcileLocked's, since it has
// to run unconditionally on every exit, independent of whatever action
// that next reconcile pass decides on. Takes the state lock itself — for
// the exit-watcher goroutine, which holds nothing.
//
// An exit caused by `stop` or `delete` is not a crash: both mark the app
// unmanaged before signaling it, so by the time this runs for that exit
// there's nothing to record — writing one would show as a phantom
// `CRASHES 1` in status, or (for delete) recreate the state file that was
// just removed.
//
// pid is the process that exited. The exit is recorded only if state.json
// still names that PID: a record that a re-apply or restart replaced or
// cleared must not take the old process's exit, or it shows up as a
// phantom crash against the replacement. The result says whether the exit
// was recorded; callers react (reconcile) only when it was.
func recordExit(spec manifest.AppSpec, pid int, exit process.Exit) bool {
	if !appRegistry.Managed(spec.AppStateFile) {
		return false
	}

	unlock := state.Lock(spec.AppStateFile)
	defer unlock()

	currentState, err := state.Load(spec.AppStateFile)
	if err != nil {
		log.Printf("reconcile: %q: recording exit: %v", spec.AppName, err)
		return false
	}
	if currentState.App().PID != pid {
		return false
	}

	recordExitLocked(spec, exit)
	return true
}

// recordExitLocked is recordExit for a caller that already holds the
// state lock — reconcileLocked, recording a port-check or Start failure
// mid-cycle.
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
