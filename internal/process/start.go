package process

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

// Exit reports how and when a started process ended.
type Exit struct {
	At     time.Time
	Failed bool // true unless the process exited cleanly (status 0)
}

// Start launches spec's R process and returns its initial state, plus a
// channel that receives a single Exit the instant the process exits — for
// any reason, crash or otherwise. The caller can block on it to react to
// death immediately instead of waiting for the next periodic liveness
// check.
func Start(spec manifest.AppSpec) (state.AppState, <-chan Exit, error) {
	command := fmt.Sprintf(
		"shiny::runApp(%q, port=%d, host=%q)",
		spec.EntrypointFile,
		spec.Port,
		spec.EffectiveHost(),
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
		Port:           spec.Port,
		EntrypointFile: spec.EntrypointFile,
		StartedAt:      time.Now(),
	}, done, nil
}
