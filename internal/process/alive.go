package process

import (
	"os/exec"
	"strconv"
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
// portably (via `ps`, no /proc dependency, so it works on macOS too,
// unlike reading the OS's own process-start-time).
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
// fallback. Unlike IsAlive it never skips the command-line check: with
// nothing to match, the answer is no. It gates every signal, where a false
// "yes" would kill a stranger and a false "no" only leaves a process alone.
func isOurs(app state.AppState, fallbacks ...string) bool {
	if !pidRunning(app) {
		return false
	}

	if app.EntrypointFile != "" {
		return HasCommandLine(app.PID, app.EntrypointFile)
	}
	for _, f := range fallbacks {
		if f != "" && HasCommandLine(app.PID, f) {
			return true
		}
	}
	return false
}

// HasCommandLine reports whether the process at pid's command line
// contains substr (shinyctl-api also uses it to recognise the load
// balancer's process). Treats a failure to even look it up (e.g. the process
// exited in the gap between the liveness check above and this call) as
// "can't confirm identity" — false, the conservative choice given this
// exists specifically to avoid false positives.
func HasCommandLine(pid int, substr string) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), substr)
}
