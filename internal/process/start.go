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

// Start launches spec's R process on InternalHost:port and returns its
// initial state (copy 1, on that port), plus a channel that receives a
// single Exit the instant the process exits — for any reason, crash or
// otherwise. The caller can block on it to react to death immediately
// instead of waiting for the next periodic liveness check.
func Start(spec manifest.AppSpec, port int) (state.AppState, <-chan Exit, error) {
	command := fmt.Sprintf(
		"shiny::runApp(%q, port=%d, host=%q)",
		spec.EntrypointFile,
		port,
		InternalHost,
	)

	proc := exec.Command(
		"Rscript",
		"-e",
		command,
	)
	proc.Dir = spec.WorkingDir
	proc.Env = buildEnv(spec.Env)

	logPath, err := AppLogPath(spec)
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
		Copy:           1,
		Port:           port,
		EntrypointFile: spec.EntrypointFile,
		StartedAt:      time.Now(),
		StartedFrom:    spec.StartFields(),
	}, done, nil
}
