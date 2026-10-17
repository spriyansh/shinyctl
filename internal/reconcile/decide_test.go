package reconcile

import (
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

func TestDecide(t *testing.T) {
	now := time.Now()

	cases := []struct {
		name    string
		spec    manifest.AppSpec
		current state.State
		alive   bool
		want    ActionType
	}{
		{
			name:    "matching state, alive is a no-op",
			spec:    manifest.AppSpec{AppName: "a", Port: 8881},
			current: state.State{Apps: []state.AppState{{Name: "a", Port: 8881}}},
			alive:   true,
			want:    ActionNone,
		},
		{
			name:    "matching name/port but dead process restarts",
			spec:    manifest.AppSpec{AppName: "a", Port: 8881},
			current: state.State{Apps: []state.AppState{{Name: "a", Port: 8881}}},
			alive:   false,
			want:    ActionStart,
		},
		{
			name:    "empty state starts the app",
			spec:    manifest.AppSpec{AppName: "a", Port: 8881},
			current: state.State{},
			alive:   false,
			want:    ActionStart,
		},
		{
			name:    "port mismatch starts the app",
			spec:    manifest.AppSpec{AppName: "a", Port: 9999},
			current: state.State{Apps: []state.AppState{{Name: "a", Port: 8881}}},
			alive:   true,
			want:    ActionStart,
		},
		{
			name:    "name mismatch starts the app",
			spec:    manifest.AppSpec{AppName: "b", Port: 8881},
			current: state.State{Apps: []state.AppState{{Name: "a", Port: 8881}}},
			alive:   true,
			want:    ActionStart,
		},
		{
			name: "first crash with no elapsed time backs off",
			spec: manifest.AppSpec{AppName: "a", Port: 8881},
			current: state.State{Apps: []state.AppState{{
				Name: "a", Port: 8881,
				CrashCount: 1, LastExitAt: now, LastExitFailed: true,
			}}},
			alive: false,
			want:  ActionBackoff,
		},
		{
			name: "restarts once the backoff delay has elapsed",
			spec: manifest.AppSpec{AppName: "a", Port: 8881},
			current: state.State{Apps: []state.AppState{{
				Name: "a", Port: 8881,
				CrashCount: 1, LastExitAt: now.Add(-backoffBase - time.Second), LastExitFailed: true,
			}}},
			alive: false,
			want:  ActionStart,
		},
		{
			name: "later crash waits out a longer backoff",
			spec: manifest.AppSpec{AppName: "a", Port: 8881},
			current: state.State{Apps: []state.AppState{{
				Name: "a", Port: 8881,
				// 2nd crash's delay (4s) hasn't elapsed, even though the
				// 1st crash's delay (2s) would have.
				CrashCount: 2, LastExitAt: now.Add(-3 * time.Second), LastExitFailed: true,
			}}},
			alive: false,
			want:  ActionBackoff,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Decide(c.spec, c.current, c.alive, now)
			if got.Type != c.want {
				t.Errorf("got %v, want %v", got.Type, c.want)
			}
		})
	}
}

// TestDecideRestartPolicy builds its state through RecordExit instead of
// by hand, so it can only ever test states the real exit path produces.
// The hand-built version of these cases used CrashCount: 1 alongside a
// clean exit — a combination RecordExit never emits (it zeroes CrashCount
// on a clean exit) — and so passed while `never` and `on-failure` were
// in fact restarting every clean exit.
func TestDecideRestartPolicy(t *testing.T) {
	now := time.Now()
	exitedAt := now.Add(-time.Hour) // every backoff delay has long elapsed

	cases := []struct {
		name   string
		policy manifest.RestartPolicy
		failed bool
		want   ActionType
	}{
		{"always restarts a crash", manifest.RestartAlways, true, ActionStart},
		{"always restarts a clean exit", manifest.RestartAlways, false, ActionStart},
		{"on-failure restarts a crash", manifest.RestartOnFailure, true, ActionStart},
		{"on-failure leaves a clean exit alone", manifest.RestartOnFailure, false, ActionNone},
		{"never gives up after a crash", manifest.RestartNever, true, ActionNone},
		{"never gives up after a clean exit", manifest.RestartNever, false, ActionNone},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := manifest.AppSpec{AppName: "a", Port: 8881, RestartPolicy: c.policy}
			app := state.AppState{Name: "a", Port: 8881, PID: 123, StartedAt: exitedAt.Add(-time.Second)}
			app = RecordExit(app, process.Exit{At: exitedAt, Failed: c.failed})

			got := Decide(spec, state.State{Apps: []state.AppState{app}}, false, now)
			if got.Type != c.want {
				t.Errorf("got %v, want %v", got.Type, c.want)
			}
		})
	}
}

func TestRecordExit(t *testing.T) {
	now := time.Now()

	t.Run("clean exit resets the crash count", func(t *testing.T) {
		app := state.AppState{StartedAt: now.Add(-time.Second), CrashCount: 3}
		got := RecordExit(app, process.Exit{At: now, Failed: false})
		if got.CrashCount != 0 {
			t.Errorf("CrashCount = %d, want 0", got.CrashCount)
		}
	})

	t.Run("fast consecutive crashes accumulate", func(t *testing.T) {
		app := state.AppState{StartedAt: now.Add(-time.Second), CrashCount: 1}
		got := RecordExit(app, process.Exit{At: now, Failed: true})
		if got.CrashCount != 2 {
			t.Errorf("CrashCount = %d, want 2", got.CrashCount)
		}
	})

	t.Run("a crash after sustained uptime starts a fresh count", func(t *testing.T) {
		app := state.AppState{StartedAt: now.Add(-time.Hour), CrashCount: 5}
		got := RecordExit(app, process.Exit{At: now, Failed: true})
		if got.CrashCount != 1 {
			t.Errorf("CrashCount = %d, want 1", got.CrashCount)
		}
	})

	t.Run("repeated start failures escalate instead of resetting to 1", func(t *testing.T) {
		// StartedAt stays zero across every call here, exactly like a
		// process that never once successfully started — regression test
		// for the bug where exit.At.Sub(zero time) is huge, so the
		// sustained-uptime branch fired on every failure and CrashCount
		// never climbed past 1.
		app := state.AppState{}
		app = RecordExit(app, process.Exit{At: now, Failed: true})
		app = RecordExit(app, process.Exit{At: now, Failed: true})
		app = RecordExit(app, process.Exit{At: now, Failed: true})
		if app.CrashCount != 3 {
			t.Errorf("CrashCount = %d, want 3", app.CrashCount)
		}
	})
}
