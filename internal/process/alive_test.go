package process

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

func TestIsAlive(t *testing.T) {
	cases := []struct {
		name string
		spec manifest.AppSpec
		app  state.AppState
		want bool
	}{
		{
			name: "our own running process is alive, no identity check requested",
			spec: manifest.AppSpec{},
			app:  state.AppState{PID: os.Getpid(), StartedAt: time.Now()},
			want: true,
		},
		{
			name: "no OS hands out PIDs this large",
			spec: manifest.AppSpec{},
			app:  state.AppState{PID: 999999999, StartedAt: time.Now()},
			want: false,
		},
		{
			name: "zero-value AppState (never started) is not alive",
			spec: manifest.AppSpec{},
			app:  state.AppState{},
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsAlive(c.spec, c.app); got != c.want {
				t.Errorf("IsAlive(%+v, %+v) = %v, want %v", c.spec, c.app, got, c.want)
			}
		})
	}
}

// TestIsAlive_PIDReuse proves the identity check closes the PID-reuse gap:
// a genuinely-alive PID that isn't running the expected entrypoint must be
// reported as not alive — exactly what happens if a dead app's old PID
// gets recycled by the OS for something else entirely.
func TestIsAlive_PIDReuse(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found on PATH, skipping")
	}

	cmd := exec.Command(sleepPath, "2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleep: %v", err)
	}
	defer cmd.Process.Kill()
	go cmd.Wait()

	app := state.AppState{PID: cmd.Process.Pid, StartedAt: time.Now()}

	t.Run("matching command line is alive", func(t *testing.T) {
		spec := manifest.AppSpec{EntrypointFile: "sleep"}
		if !IsAlive(spec, app) {
			t.Error("IsAlive = false, want true (command line contains \"sleep\")")
		}
	})

	t.Run("recorded entrypoint wins over the spec's", func(t *testing.T) {
		spec := manifest.AppSpec{EntrypointFile: "some-other-app.R"}
		recorded := app
		recorded.EntrypointFile = "sleep"
		if !IsAlive(spec, recorded) {
			t.Error("IsAlive = false, want true (the recorded entrypoint matches the running command)")
		}
	})

	t.Run("recorded entrypoint that does not match is not alive, even if the spec's does", func(t *testing.T) {
		spec := manifest.AppSpec{EntrypointFile: "sleep"}
		recorded := app
		recorded.EntrypointFile = "some-other-app.R"
		if IsAlive(spec, recorded) {
			t.Error("IsAlive = true, want false (the recorded entrypoint is not what is running)")
		}
	})

	t.Run("mismatched entrypoint is not alive, even though the PID is real", func(t *testing.T) {
		spec := manifest.AppSpec{EntrypointFile: "some-other-app.R"}
		if IsAlive(spec, app) {
			t.Error("IsAlive = true, want false (PID is real, but running a different command)")
		}
	})
}
