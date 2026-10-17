package reconcile

import (
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

// crashResetWindow is how long a process has to stay up for its next exit
// to be treated as a fresh problem rather than a continuation of a crash
// loop. Mirrors Kubernetes' own CrashLoopBackOff reset-after-sustained-
// uptime behavior: an app that ran fine for an hour and then crashed once
// shouldn't inherit backoff from crashes it had days ago.
const crashResetWindow = 60 * time.Second

// backoffBase and backoffMax bound the delay RecordExit's CrashCount
// produces: 2s, 4s, 8s, ... capped at 1 minute, so a tight crash loop
// settles into one restart attempt a minute instead of pegging a CPU core.
const (
	backoffBase = 2 * time.Second
	backoffMax  = 60 * time.Second
)

// RecordExit folds one process exit into app's crash-tracking fields. It's
// a pure function deliberately kept separate from Decide: an exit is a
// fact that happened once and must be persisted, whereas Decide re-runs on
// every reconcile tick and must stay re-derivable from whatever's already
// on disk.
func RecordExit(app state.AppState, exit process.Exit) state.AppState {
	app.LastExitAt = exit.At
	app.LastExitFailed = exit.Failed

	switch {
	case !exit.Failed:
		app.CrashCount = 0
	// StartedAt is zero when the process never actually started (a pure
	// process.Start failure — bad working-dir, missing Rscript slipping
	// past Validate, etc.), not just whenever it's "old": a never-started
	// app has nothing to measure sustained uptime against, so treating a
	// repeated Start failure as "ran fine for a while, fresh problem"
	// would reset CrashCount to 1 on every single failure forever,
	// silently defeating escalation for exactly the case this handles.
	case !app.StartedAt.IsZero() && exit.At.Sub(app.StartedAt) >= crashResetWindow:
		app.CrashCount = 1
	default:
		app.CrashCount++
	}

	return app
}

// backoffDelay returns how long to wait before the next restart attempt
// after crashCount consecutive fast crashes.
func backoffDelay(crashCount int) time.Duration {
	if crashCount <= 0 {
		return 0
	}

	delay := backoffBase
	for i := 1; i < crashCount; i++ {
		delay *= 2
		if delay >= backoffMax {
			return backoffMax
		}
	}

	return delay
}

// Decide is the reconcile loop's pure decision function: given what spec
// wants, what state.json last recorded, and a freshly-probed alive bool,
// it says what to do — never does it itself. now is the caller's
// time.Now(), passed in rather than read here so this stays deterministic
// and table-testable.
func Decide(spec manifest.AppSpec, current state.State, alive bool, now time.Time) Action {
	app := current.App()

	if app.Name == spec.AppName && app.Port == spec.Port && alive {
		return Action{Type: ActionNone} // Spec left as zero value, unused
	}

	// Gate the policy on "has this app ever exited," not on CrashCount:
	// RecordExit zeroes CrashCount on a clean exit, so keying off it would
	// make `never` and `on-failure` restart precisely the exits they exist
	// to leave alone. LastExitAt is the one field every exit sets.
	if !app.LastExitAt.IsZero() {
		switch spec.RestartPolicy.Effective() {
		case manifest.RestartNever:
			return Action{Type: ActionNone}
		case manifest.RestartOnFailure:
			if !app.LastExitFailed {
				return Action{Type: ActionNone}
			}
		}

		if now.Sub(app.LastExitAt) < backoffDelay(app.CrashCount) {
			return Action{Type: ActionBackoff, Spec: spec}
		}
	}

	return Action{Type: ActionStart, Spec: spec} // "start, and here's what to start"
}
