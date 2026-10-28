package reconcile

import (
	"reflect"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

// fullSpec and the helpers below build records the way process.Start does:
// a live app started from a spec, with everything recorded.
var fullSpec = manifest.AppSpec{
	AppGroup:       "g",
	AppName:        "a",
	EntrypointFile: "/srv/app.R",
	Host:           "127.0.0.1",
	Port:           8881,
	WorkingDir:     "/srv",
	Env:            map[string]string{"A": "1"},
}

func withSpec(s manifest.AppSpec, mutate func(*manifest.AppSpec)) manifest.AppSpec {
	mutate(&s)
	return s
}

// startedFrom is the record of an app started from spec after mutate was
// applied to it (nil for none), so a test can compare it with a different
// current spec.
func startedFrom(spec manifest.AppSpec, mutate func(*manifest.AppSpec)) state.AppState {
	if mutate != nil {
		mutate(&spec)
	}
	return state.AppState{
		Name:           spec.AppName,
		Copy:           1,
		Port:           20000, // internal: not the spec's port
		EntrypointFile: spec.EntrypointFile,
		StartedFrom:    spec.StartFields(),
	}
}

// decide is what DecideCopies makes of an app with one copy (no spares),
// the shape every app had before copies: the plan, for a copy recorded in
// current and alive or not.
func decide(spec manifest.AppSpec, current state.State, alive bool, now time.Time) Plan {
	spec.Replicas, spec.WarmReplicas = 1, intPtr(0)
	var views []CopyView
	for _, app := range current.Apps {
		views = append(views, CopyView{App: app, Alive: alive, Ready: alive})
	}
	return DecideCopies(spec, current.Revision, views, now)
}

func TestDecideOneCopy(t *testing.T) {
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
			current: state.State{Apps: []state.AppState{{Name: "a", Copy: 1, Port: 20000}}},
			alive:   true,
			want:    ActionNone,
		},
		{
			name:    "matching name/port but dead process restarts",
			spec:    manifest.AppSpec{AppName: "a", Port: 8881},
			current: state.State{Apps: []state.AppState{{Name: "a", Copy: 1, Port: 20000}}},
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
			// The record's port is internal: the manifest's port moves with
			// no restart.
			name:    "a record port that differs from the manifest port is left alone",
			spec:    manifest.AppSpec{AppName: "a", Port: 9999},
			current: state.State{Apps: []state.AppState{{Name: "a", Copy: 1, Port: 20000}}},
			alive:   true,
			want:    ActionNone,
		},
		{
			name:    "a host change alone leaves a live process alone",
			spec:    withSpec(fullSpec, func(s *manifest.AppSpec) { s.Host = "0.0.0.0" }),
			current: state.State{Apps: []state.AppState{startedFrom(fullSpec, nil)}},
			alive:   true,
			want:    ActionNone,
		},
		{
			name:    "a live process from before the load balancer is moved behind it",
			spec:    manifest.AppSpec{AppName: "a", Port: 8881},
			current: state.State{Apps: []state.AppState{{Name: "a", Port: 8881}}},
			alive:   true,
			want:    ActionStart,
		},
		{
			name: "a dead process under restart-policy never that has exited is left alone",
			spec: manifest.AppSpec{AppName: "a", Port: 8881, RestartPolicy: manifest.RestartNever},
			current: state.State{Apps: []state.AppState{{
				Name: "a", Copy: 1, Port: 20000, LastExitAt: now.Add(-time.Hour),
			}}},
			alive: false,
			want:  ActionNone,
		},
		{
			name:    "name mismatch starts the app",
			spec:    manifest.AppSpec{AppName: "b", Port: 8881},
			current: state.State{Apps: []state.AppState{{Name: "a", Copy: 1, Port: 20000}}},
			alive:   true,
			want:    ActionStart,
		},
		{
			name: "first crash with no elapsed time backs off",
			spec: manifest.AppSpec{AppName: "a", Port: 8881},
			current: state.State{Apps: []state.AppState{{
				Name: "a", Copy: 1, Port: 20000,
				CrashCount: 1, LastExitAt: now, LastExitFailed: true,
			}}},
			alive: false,
			want:  ActionBackoff,
		},
		{
			name: "restarts once the backoff delay has elapsed",
			spec: manifest.AppSpec{AppName: "a", Port: 8881},
			current: state.State{Apps: []state.AppState{{
				Name: "a", Copy: 1, Port: 20000,
				CrashCount: 1, LastExitAt: now.Add(-backoffBase - time.Second), LastExitFailed: true,
			}}},
			alive: false,
			want:  ActionStart,
		},
		{
			name: "later crash waits out a longer backoff",
			spec: manifest.AppSpec{AppName: "a", Port: 8881},
			current: state.State{Apps: []state.AppState{{
				Name: "a", Copy: 1, Port: 20000,
				// 2nd crash's delay (4s) hasn't elapsed, even though the
				// 1st crash's delay (2s) would have.
				CrashCount: 2, LastExitAt: now.Add(-3 * time.Second), LastExitFailed: true,
			}}},
			alive: false,
			want:  ActionBackoff,
		},
		{
			name:    "live process started from an older env is replaced",
			spec:    fullSpec,
			current: state.State{Apps: []state.AppState{startedFrom(fullSpec, func(s *manifest.AppSpec) { s.Env = map[string]string{"A": "old"} })}},
			alive:   true,
			want:    ActionStart,
		},
		{
			name: "live process with a changed name is replaced despite a recent crash",
			spec: manifest.AppSpec{AppName: "b", Port: 8881},
			current: state.State{Apps: []state.AppState{{
				Name: "a", Copy: 1, Port: 20000,
				CrashCount: 1, LastExitAt: now.Add(-time.Second), LastExitFailed: true,
			}}},
			alive: true,
			want:  ActionStart,
		},
		{
			name: "live process with a changed name is replaced under restart-policy never",
			spec: manifest.AppSpec{AppName: "b", Port: 8881, RestartPolicy: manifest.RestartNever},
			current: state.State{Apps: []state.AppState{{
				Name: "a", Copy: 1, Port: 20000,
				LastExitAt: now.Add(-time.Hour),
			}}},
			alive: true,
			want:  ActionStart,
		},
		{
			name: "live process from an old record is left alone despite a changed env",
			spec: withSpec(fullSpec, func(s *manifest.AppSpec) { s.Env = map[string]string{"A": "2"} }),
			current: state.State{Apps: []state.AppState{{
				Name: "a", Copy: 1, Port: 20000, // no entrypoint, no StartedFrom
			}}},
			alive: true,
			want:  ActionNone,
		},
		{
			name:    "a changed restart-policy alone leaves a live process alone",
			spec:    withSpec(fullSpec, func(s *manifest.AppSpec) { s.RestartPolicy = manifest.RestartNever }),
			current: state.State{Apps: []state.AppState{startedFrom(fullSpec, nil)}},
			alive:   true,
			want:    ActionNone,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decide(c.spec, c.current, c.alive, now).Action(); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestDecideOneCopy_UpgradeFlag(t *testing.T) {
	spec := manifest.AppSpec{AppName: "a", Port: 8881}
	old := state.State{Apps: []state.AppState{{Name: "a", Port: 8881, PID: 7}}}
	got := decide(spec, old, true, time.Now())
	if want := []string{"stop 0 pid=7", "start 1 upgrade"}; !reflect.DeepEqual(show(got.Steps), want) {
		t.Errorf("alive record without a copy: steps %q, want %q", show(got.Steps), want)
	}

	current := state.State{Apps: []state.AppState{{Name: "b", Copy: 1, Port: 20000, PID: 8}}}
	got = decide(spec, current, true, time.Now())
	if want := []string{"start 1 pid=8 replace changed=[app-name]"}; !reflect.DeepEqual(show(got.Steps), want) {
		t.Errorf("alive copy with a changed name: steps %q, want %q", show(got.Steps), want)
	}

	got = decide(spec, old, false, time.Now())
	if want := []string{"start 1"}; !reflect.DeepEqual(show(got.Steps), want) {
		t.Errorf("dead record without a copy: steps %q, want %q (a plain start)", show(got.Steps), want)
	}
}

func TestDecideOneCopy_ReportsChangedFields(t *testing.T) {
	spec := withSpec(fullSpec, func(s *manifest.AppSpec) { s.Env = map[string]string{"A": "new"} })
	rec := startedFrom(fullSpec, nil)
	rec.PID = 9
	current := state.State{Apps: []state.AppState{rec}}

	got := decide(spec, current, true, time.Now())
	if want := []string{"start 1 pid=9 replace changed=[env]"}; !reflect.DeepEqual(show(got.Steps), want) {
		t.Errorf("steps %q, want %q", show(got.Steps), want)
	}
}

func TestGaveUp(t *testing.T) {
	exited := func(failed bool) state.AppState {
		return state.AppState{LastExitAt: time.Now(), LastExitFailed: failed}
	}

	cases := []struct {
		name   string
		policy manifest.RestartPolicy
		app    state.AppState
		want   bool
	}{
		{"never exited, always", manifest.RestartAlways, state.AppState{}, false},
		{"never exited, never", manifest.RestartNever, state.AppState{}, false},
		{"crashed, always", manifest.RestartAlways, exited(true), false},
		{"crashed, unset policy", "", exited(true), false},
		{"clean exit, always", manifest.RestartAlways, exited(false), false},
		{"crashed, on-failure", manifest.RestartOnFailure, exited(true), false},
		{"clean exit, on-failure", manifest.RestartOnFailure, exited(false), true},
		{"crashed, never", manifest.RestartNever, exited(true), true},
		{"clean exit, never", manifest.RestartNever, exited(false), true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := manifest.AppSpec{RestartPolicy: c.policy}
			if got := GaveUp(spec, c.app); got != c.want {
				t.Errorf("GaveUp() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDecideOneCopyRestartPolicy builds its state through RecordExit instead of
// by hand, so it can only ever test states the real exit path produces.
// The hand-built version of these cases used CrashCount: 1 alongside a
// clean exit — a combination RecordExit never emits (it zeroes CrashCount
// on a clean exit) — and so passed while `never` and `on-failure` were
// in fact restarting every clean exit.
func TestDecideOneCopyRestartPolicy(t *testing.T) {
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

			if got := decide(spec, state.State{Apps: []state.AppState{app}}, false, now).Action(); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
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

func TestChangedFields(t *testing.T) {
	record := startedFrom(fullSpec, nil)

	cases := []struct {
		name   string
		mutate func(*manifest.AppSpec)
		app    state.AppState
		want   []string
	}{
		{"identical", func(s *manifest.AppSpec) {}, record, nil},
		{"app-group", func(s *manifest.AppSpec) { s.AppGroup = "h" }, record, []string{"app-group"}},
		{"app-name", func(s *manifest.AppSpec) { s.AppName = "b" }, record, []string{"app-name"}},
		{"entrypoint-file", func(s *manifest.AppSpec) { s.EntrypointFile = "/srv/other.R" }, record, []string{"entrypoint-file"}},
		{"port is not a start field", func(s *manifest.AppSpec) { s.Port = 9999 }, record, nil},
		{"host is not a start field", func(s *manifest.AppSpec) { s.Host = "0.0.0.0" }, record, nil},
		{"working-dir", func(s *manifest.AppSpec) { s.WorkingDir = "/elsewhere" }, record, []string{"working-dir"}},
		{"env", func(s *manifest.AppSpec) { s.Env = map[string]string{"A": "2"} }, record, []string{"env"}},
		{"restart-policy is not a start field", func(s *manifest.AppSpec) { s.RestartPolicy = manifest.RestartNever }, record, nil},
		{"replicas is not a start field", func(s *manifest.AppSpec) { s.Replicas = 1 }, record, nil},
		{
			"several, in manifest order",
			func(s *manifest.AppSpec) {
				s.Env = map[string]string{"A": "2"}
				s.WorkingDir = "/elsewhere"
				s.Host = "0.0.0.0"
				s.Port = 9999
				s.AppGroup = "h"
			},
			record,
			[]string{"app-group", "working-dir", "env"},
		},
		{
			"old record: only the name is known",
			func(s *manifest.AppSpec) {
				s.AppGroup = "h"
				s.Host = "0.0.0.0"
				s.WorkingDir = "/elsewhere"
				s.Env = map[string]string{"A": "2"}
				s.EntrypointFile = "/srv/other.R"
			},
			state.AppState{Name: "a", Port: 8881},
			nil,
		},
		{
			"old record: an edited entrypoint is not a restart",
			func(s *manifest.AppSpec) { s.EntrypointFile = "/srv/other.R" },
			state.AppState{Name: "a", Port: 8881, EntrypointFile: "/srv/app.R"},
			nil,
		},
		{"old record: a changed name is still seen", func(s *manifest.AppSpec) { s.AppName = "b" }, state.AppState{Name: "a", Port: 8881}, []string{"app-name"}},
		{
			"partial record: an unrecorded env is unknown",
			func(s *manifest.AppSpec) { s.Env = map[string]string{"A": "2"} },
			state.AppState{Name: "a", Port: 8881, StartedFrom: map[string]string{"app-group": "g"}},
			nil,
		},
		{
			"partial record: a recorded app-group is compared",
			func(s *manifest.AppSpec) { s.AppGroup = "h" },
			state.AppState{Name: "a", Port: 8881, StartedFrom: map[string]string{"app-group": "g"}},
			[]string{"app-group"},
		},
		{
			"a host recorded by an earlier version is ignored",
			func(s *manifest.AppSpec) { s.Host = "0.0.0.0" },
			state.AppState{Name: "a", Port: 8881, StartedFrom: map[string]string{"host": "127.0.0.1"}},
			nil,
		},
		{
			"a key from a newer version is ignored",
			func(s *manifest.AppSpec) {},
			func() state.AppState {
				a := startedFrom(fullSpec, nil)
				a.StartedFrom["future-field"] = "x"
				return a
			}(),
			nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := withSpec(fullSpec, c.mutate)
			if got := changedFields(c.app, spec); !reflect.DeepEqual(got, c.want) {
				t.Errorf("changedFields() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSettingsChanged(t *testing.T) {
	cases := []struct {
		name     string
		from, to manifest.AppSpec
		want     []string
	}{
		{"unset and always are the same", manifest.AppSpec{}, manifest.AppSpec{RestartPolicy: manifest.RestartAlways}, nil},
		{"always to never", manifest.AppSpec{RestartPolicy: manifest.RestartAlways}, manifest.AppSpec{RestartPolicy: manifest.RestartNever}, []string{"restart-policy"}},
		{"replicas omitted to 1", manifest.AppSpec{}, manifest.AppSpec{Replicas: 1}, nil},
		{"drain-timeout unset and 15m are the same", manifest.AppSpec{}, manifest.AppSpec{DrainTimeout: "15m"}, nil},
		{"drain-timeout written another way is no change", manifest.AppSpec{DrainTimeout: "15m"}, manifest.AppSpec{DrainTimeout: "900s"}, nil},
		{"drain-timeout 15m to 5m", manifest.AppSpec{DrainTimeout: "15m"}, manifest.AppSpec{DrainTimeout: "5m"}, []string{"drain-timeout"}},
		{"drain-timeout comes after lb-policy", manifest.AppSpec{LBPolicy: "first"}, manifest.AppSpec{LBPolicy: "random", DrainTimeout: "0"}, []string{"lb-policy", "drain-timeout"}},
		{"port", manifest.AppSpec{Port: 8080}, manifest.AppSpec{Port: 8081}, []string{"port"}},
		{"host", manifest.AppSpec{Host: "127.0.0.1"}, manifest.AppSpec{Host: "0.0.0.0"}, []string{"host"}},
		{"the same address written another way is no change", manifest.AppSpec{Host: "127.0.0.1"}, manifest.AppSpec{Host: "::ffff:127.0.0.1"}, nil},
		{"omitted host and 0.0.0.0 are the same", manifest.AppSpec{}, manifest.AppSpec{Host: "0.0.0.0"}, nil},
		{
			"port, host and restart-policy together, in manifest order",
			manifest.AppSpec{Port: 8080, Host: "127.0.0.1"},
			manifest.AppSpec{Port: 8081, Host: "0.0.0.0", RestartPolicy: manifest.RestartNever},
			[]string{"port", "host", "restart-policy"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SettingsChanged(c.from, c.to); !reflect.DeepEqual(got, c.want) {
				t.Errorf("SettingsChanged() = %v, want %v", got, c.want)
			}
		})
	}
}
