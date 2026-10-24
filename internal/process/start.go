package process

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

// InternalHost is the address an app's R process binds. It is behind the
// load balancer, which listens on the app's own host and port, so nothing
// else on the network can reach the process directly.
const InternalHost = "127.0.0.1"

// Exit reports how and when a started process ended.
type Exit struct {
	At     time.Time
	Failed bool // true unless the process exited cleanly (status 0)
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
		err := proc.Wait()
		done <- Exit{At: time.Now(), Failed: err != nil}
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
