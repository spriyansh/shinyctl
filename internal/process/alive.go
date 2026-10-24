package process

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

// IsAlive reports whether app's recorded process is genuinely still
// running — not just whether some process exists at that PID. A dead
// process's PID can be recycled by the OS for an unrelated process;
// syscall.Kill(pid, 0) alone can't tell the difference. Checking the
// running command line against the entrypoint closes that gap cheaply and
// portably (from /proc on Linux and from `ps` elsewhere, in either case read
// without regard to the locale; see commandLine), unlike reading the OS's own
// process-start-time.
//
// The entrypoint recorded in app wins over spec's: after a re-apply, spec
// describes the replacement, and the recorded process must still be
// recognised so it can be stopped instead of orphaned. spec's entrypoint
// is the fallback for records written before Start recorded one. If both
// are empty, the identity check is skipped — callers without a spec handy
// still get the plain PID check.
func IsAlive(spec manifest.AppSpec, app state.AppState) bool {
	if app.EntrypointFile == "" && spec.EntrypointFile == "" {
		return pidRunning(app) // callers without a spec still get the plain PID check
	}
	return isOurs(app, spec.EntrypointFile)
}

// PIDRunning reports whether app's recorded PID exists, without saying
// whose process it is. Callers use it to tell "nothing is running" from
// "something is, but it isn't the app" after Stop declines to signal.
func PIDRunning(app state.AppState) bool {
	return pidRunning(app)
}

// pidRunning reports whether app's recorded PID exists, without saying
// whose process it is.
func pidRunning(app state.AppState) bool {
	// PID 0 means "never actually recorded as started" for a zero-value
	// AppState — and syscall.Kill(0, 0) is not a safe no-op: PID 0 signals
	// the caller's entire process group, so it would report nil/alive here
	// for a PID that was never ours. StartedAt.IsZero() catches the same
	// case from the other direction.
	if app.PID <= 0 || app.StartedAt.IsZero() {
		return false
	}

	err := syscall.Kill(app.PID, 0)
	return err == nil || err == syscall.EPERM
}

// isOurs reports whether app's recorded process is still running and is
// identifiably the one shinyctl started: its command line contains
// app.EntrypointFile or, for a record without one, any non-empty
// fallback, and the port the record names. Unlike IsAlive it never skips
// the command-line check: with nothing to match, the answer is no. It gates
// every signal, where a false "yes" would kill a stranger and a false "no"
// only leaves a process alone.
func isOurs(app state.AppState, fallbacks ...string) bool {
	if !pidRunning(app) {
		return false
	}
	line, ok := commandLine(app.PID)
	return ok && matches(app, line, fallbacks...)
}

// matches reports whether line, the command line of the process at app's
// PID, is app's. The entrypoint alone can't tell the copies of one app
// apart once a PID is reused, so the port the process was started on must
// be in the line too (Start writes it as `port=<port>,`).
func matches(app state.AppState, line string, fallbacks ...string) bool {
	if app.Port > 0 && !strings.Contains(line, fmt.Sprintf("port=%d,", app.Port)) {
		return false
	}
	if app.EntrypointFile != "" {
		return inCommandLine(line, app.EntrypointFile)
	}
	for _, f := range fallbacks {
		if f != "" && inCommandLine(line, f) {
			return true
		}
	}
	return false
}

// AliveAll is IsAlive for each of apps, with the command lines of all of them
// read together: from /proc on Linux, with one ps call elsewhere. The result
// is in the order of apps.
func AliveAll(spec manifest.AppSpec, apps []state.AppState) []bool {
	alive := make([]bool, len(apps))
	var pids []int
	for _, app := range apps {
		if pidRunning(app) {
			pids = append(pids, app.PID)
		}
	}
	if len(pids) == 0 {
		return alive
	}

	lines := commandLines(pids)
	for i, app := range apps {
		if !pidRunning(app) {
			continue
		}
		if app.EntrypointFile == "" && spec.EntrypointFile == "" {
			alive[i] = true // no identity to check, as in IsAlive
			continue
		}
		line, ok := lines[app.PID]
		alive[i] = ok && matches(app, line, spec.EntrypointFile)
	}
	return alive
}

// HasCommandLine reports whether the process at pid's command line
// contains substr (shinyctl-api also uses it to recognise the load
// balancer's process). Treats a failure to even look it up (e.g. the process
// exited in the gap between the liveness check above and this call) as
// "can't confirm identity" — false, the conservative choice given this
// exists specifically to avoid false positives.
func HasCommandLine(pid int, substr string) bool {
	line, ok := commandLine(pid)
	return ok && inCommandLine(line, substr)
}
