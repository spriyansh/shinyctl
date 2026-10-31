package process

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

// InternalHost is the address an app's R process binds. It is behind the
// load balancer, which listens on the app's own host and port, so nothing
// else on the network can reach the process directly.
const InternalHost = "127.0.0.1"

// ExitReason says why shinyctl ended a process, when it did. Most exits have
// no reason: the process stopped or crashed on its own.
type ExitReason string

// ExitOOM is the reason for a copy that shinyctl killed for going over its
// max-memory.
const ExitOOM ExitReason = "oom"

// Exit reports how and when a started process ended.
type Exit struct {
	At     time.Time
	Failed bool // true unless the process exited cleanly (status 0)

	// Reason is empty here: Start knows only how the process ended. The caller
	// that killed it sets it, because only that caller knows why.
	Reason ExitReason

	// Signal is the signal that ended the process, or 0 when it exited by
	// itself. A kill is told from a crash by it.
	Signal syscall.Signal
}

// exitOf builds the Exit of a process that ended at the moment it is called,
// from the status Wait collected.
func exitOf(ps *os.ProcessState) Exit {
	// Wait can fail before it collects a status; all that is known is that
	// the process is not known to have exited cleanly.
	if ps == nil {
		return Exit{At: time.Now(), Failed: true}
	}
	exit := Exit{At: time.Now(), Failed: !ps.Success()}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		exit.Signal = ws.Signal()
	}
	return exit
}

// Start launches copy number copy of spec's app, an R process on
// InternalHost:port, and returns its initial state (that copy, on that port,
// started at revision), plus a channel that receives a single Exit the
// instant the process exits — for any reason, crash or otherwise. The caller
// can block on it to react to death immediately instead of waiting for the
// next periodic liveness check.
//
// The copy's log gets a marker line before R starts, so a log that holds
// several starts can be read as separate runs.
func Start(spec manifest.AppSpec, copy, port, revision int) (state.AppState, <-chan Exit, error) {
	command := RunAppExpression(spec.EntrypointFile, port, InternalHost)

	proc := exec.Command(
		"Rscript",
		"-e",
		command,
	)
	proc.Dir = spec.WorkingDir
	proc.Env = buildEnv(spec.Env)

	logPath, err := CopyLogPath(spec, copy)
	if err != nil {
		return state.AppState{}, nil, err
	}

	logFile, err := os.OpenFile(
		logPath,
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err != nil {
		return state.AppState{}, nil, err
	}

	startedAt := time.Now()
	if _, err := fmt.Fprintf(logFile, "--- shinyctl: starting copy %d at %s ---\n", copy, startedAt.Format(time.RFC3339)); err != nil {
		logFile.Close()
		return state.AppState{}, nil, err
	}

	proc.Stdout = logFile
	proc.Stderr = logFile

	// The child holds its own copy of the descriptor; the parent's would only
	// leak one per start.
	defer logFile.Close()

	if err := proc.Start(); err != nil {
		return state.AppState{}, nil, fmt.Errorf(
			"starting %q: %w",
			spec.AppName,
			err,
		)
	}

	// Reap the child whenever it exits, and signal done at the same
	// moment. Without the reap, a killed/crashed process stays a zombie
	// forever (its PID never freed, since nothing ever collects its exit
	// status) — and syscall.Kill(pid, 0) succeeds against a zombie, so
	// IsAlive would report it as alive indefinitely.
	done := make(chan Exit, 1)
	go func() {
		proc.Wait()
		done <- exitOf(proc.ProcessState)
	}()

	return state.AppState{
		Name:           spec.AppName,
		PID:            proc.Process.Pid,
		Copy:           copy,
		Port:           port,
		EntrypointFile: spec.EntrypointFile,
		StartedAt:      startedAt,
		StartedFrom:    spec.StartFields(),
		Revision:       revision,
	}, done, nil
}
