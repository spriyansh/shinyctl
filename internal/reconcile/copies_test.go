package reconcile

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

var (
	base = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	now  = base.Add(time.Hour)
)

func intPtr(n int) *int { return &n }

// copySpec is an app with replicas r and warm-replicas w.
func copySpec(r, w int) manifest.AppSpec {
	return manifest.AppSpec{
		AppGroup: "g", AppName: "app", EntrypointFile: "/srv/app.R",
		Port: 8080, WorkingDir: "/srv", Replicas: r, WarmReplicas: intPtr(w),
	}
}

// rec is the record of copy n started age after base, from spec, at
// revision rev.
func rec(spec manifest.AppSpec, n int, age time.Duration, rev int) state.AppState {
	return state.AppState{
		Name: spec.AppName, PID: 100 + n, Copy: n, Port: 20000 + n,
		EntrypointFile: spec.EntrypointFile, StartedAt: base.Add(age),
		StartedFrom: spec.StartFields(), Revision: rev,
	}
}

// ready is a copy that is alive and accepting connections.
func ready(spec manifest.AppSpec, n int, age time.Duration) CopyView {
	return CopyView{App: rec(spec, n, age, 0), Alive: true, Ready: true}
}

func startingCopy(spec manifest.AppSpec, n int, age time.Duration) CopyView {
	return CopyView{App: rec(spec, n, age, 0), Alive: true}
}

// dead is a copy whose process is gone; mutate sets its crash history.
func dead(spec manifest.AppSpec, n int, mutate func(*state.AppState)) CopyView {
	a := rec(spec, n, 0, 0)
	if mutate != nil {
		mutate(&a)
	}
	return CopyView{App: a}
}

func exited(at time.Time, crashes int) func(*state.AppState) {
	return func(a *state.AppState) {
		a.LastExitAt, a.LastExitFailed, a.CrashCount = at, true, crashes
	}
}

func withRevision(v CopyView, rev int) CopyView { v.App.Revision = rev; return v }
func withSessions(v CopyView, n int) CopyView   { v.Sessions = n; return v }
func pending(v CopyView) CopyView               { v.Alive, v.Ready, v.Pending = false, false, true; return v }
func withPolicy(s manifest.AppSpec, p manifest.RestartPolicy) manifest.AppSpec {
	s.RestartPolicy = p
	return s
}

// show writes steps as short strings for comparison.
func show(steps []Step) []string {
	var out []string
	for _, s := range steps {
		var b strings.Builder
		fmt.Fprintf(&b, "%s %d", s.Type, s.Copy)
		if s.PID != 0 {
			fmt.Fprintf(&b, " pid=%d", s.PID)
		}
		if s.Replace {
			b.WriteString(" replace")
		}
		if len(s.Changed) > 0 {
			fmt.Fprintf(&b, " changed=%v", s.Changed)
		}
		if s.Restart {
			b.WriteString(" restart")
		}
		if s.Upgrade {
			b.WriteString(" upgrade")
		}
		out = append(out, b.String())
	}
	return out
}

func TestDecideCopies(t *testing.T) {
	s11, s20, s21, s31, s10 := copySpec(1, 1), copySpec(2, 0), copySpec(2, 1), copySpec(3, 1), copySpec(1, 0)
	envChanged := withEnv(s10, "B", "2")

	cases := []struct {
		name     string
		spec     manifest.AppSpec
		revision int
		copies   []CopyView
		steps    []string
		active   []int
		warm     []int
		rolling  bool
	}{
		{name: "1 no records, 1 copy and a spare", spec: s11,
			steps: []string{"start 1", "start 2"}},
		{name: "2 no records, 2 copies", spec: s20,
			steps: []string{"start 1", "start 2"}},
		{name: "3 two ready, oldest is active", spec: s11,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			active: []int{1}, warm: []int{2}},
		{name: "4 roles follow started-at, not the copy number", spec: s11,
			copies: []CopyView{ready(s11, 1, time.Minute), ready(s11, 2, time.Second)},
			active: []int{2}, warm: []int{1}},
		{name: "5 copy 1 dead, the spare serves, copy 1 starts in place", spec: s11,
			copies: []CopyView{dead(s11, 1, exited(base, 1)), ready(s11, 2, time.Second)},
			steps:  []string{"start 1"}, active: []int{2}},
		{name: "6 copy 1 pending is left to its watcher", spec: s11,
			copies: []CopyView{pending(dead(s11, 1, nil)), ready(s11, 2, time.Second)},
			active: []int{2}},
		{name: "7 dead in backoff", spec: s11,
			copies: []CopyView{dead(s11, 1, exited(now.Add(-time.Second), 3)), ready(s11, 2, time.Second)},
			steps:  []string{"backoff 1"}, active: []int{2}},
		{name: "8 dead and given up (never): nothing, but the app is not given up", spec: withPolicy(s11, manifest.RestartNever),
			copies: []CopyView{dead(s11, 1, exited(base, 1)), ready(s11, 2, time.Second)},
			active: []int{2}},
		{name: "9 everything dead and given up: nothing to do", spec: withPolicy(s11, manifest.RestartNever),
			copies: []CopyView{dead(s11, 1, exited(base, 1)), dead(s11, 2, exited(base, 1))},
		},
		{name: "10 a copy that is not ready has no role", spec: s11,
			copies: []CopyView{ready(s11, 1, 0), startingCopy(s11, 2, time.Second)},
			active: []int{1}},
		{name: "11 scale up", spec: s21,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			steps:  []string{"start 3"}, active: []int{1, 2}},
		{name: "12 scale down: the spare goes", spec: s11,
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second)},
			steps:  []string{"stop 3 pid=103"}, active: []int{1}, warm: []int{2}},
		{name: "13 scale down: the spare, then the active with the fewest sessions", spec: s11,
			copies: []CopyView{
				withSessions(ready(s31, 1, 0), 5), withSessions(ready(s31, 2, time.Second), 0),
				withSessions(ready(s31, 3, 2*time.Second), 2), ready(s31, 4, 3*time.Second),
			},
			steps: []string{"stop 2 pid=102", "stop 4 pid=104"}, active: []int{1}, warm: []int{3}},
		{name: "14 scale down: a copy with no process goes first", spec: s11,
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), dead(s21, 3, exited(base, 1))},
			steps:  []string{"stop 3"}, active: []int{1}, warm: []int{2}},
		{name: "15 scale down: a starting copy goes before a warm one", spec: s10,
			copies: []CopyView{ready(s11, 1, 0), startingCopy(s11, 2, time.Second)},
			steps:  []string{"stop 2 pid=102"}, active: []int{1}},
		{name: "16 sparse numbers take the lowest free one", spec: copySpec(2, 1),
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 3, time.Second)},
			steps:  []string{"start 2"}, active: []int{1, 3}},
		{name: "17 outdated by env, one copy: replaced at once", spec: envChanged,
			copies: []CopyView{ready(s10, 1, 0)},
			steps:  []string{"start 1 pid=101 replace changed=[env]"}},
		{name: "18 outdated by restart: the spare first", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			steps:  []string{"start 2 pid=102 replace restart"}, active: []int{1}},
		{name: "19 outdated, but a current copy is still starting: wait", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), withRevision(startingCopy(s11, 2, time.Second), 1)},
			active: []int{1}, rolling: true},
		{name: "20 outdated, a current copy is in backoff: stall", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), withRevision(dead(s11, 2, exited(now.Add(-time.Second), 3)), 1)},
			steps:  []string{"backoff 2"}, active: []int{1}, rolling: true},
		{name: "21 outdated, a current copy gave up: stall", spec: withPolicy(s11, manifest.RestartNever), revision: 1,
			copies: []CopyView{ready(s11, 1, 0), withRevision(dead(s11, 2, exited(base, 1)), 1)},
			active: []int{1}, rolling: true},
		{name: "22 two outdated actives: the one with fewer sessions", spec: s20, revision: 1,
			copies: []CopyView{withSessions(ready(s20, 1, 0), 3), withSessions(ready(s20, 2, time.Second), 1)},
			steps:  []string{"start 2 pid=102 replace restart"}, active: []int{1}},
		{name: "23 outdated and dead: a plain start", spec: s10, revision: 1,
			copies: []CopyView{dead(s10, 1, exited(base, 1))},
			steps:  []string{"start 1"}},
		{name: "24 a process from before the load balancer", spec: s11,
			copies: []CopyView{{App: state.AppState{Name: "app", PID: 77, Port: 8080, EntrypointFile: s11.EntrypointFile, StartedAt: base}, Alive: true, Ready: true}},
			steps:  []string{"stop 0 pid=77", "start 1 upgrade", "start 2"}},
		{name: "25a dead record from before the load balancer is copy 1", spec: s10,
			copies: []CopyView{{App: state.AppState{Name: "app", PID: 77, Port: 8080, StartedAt: base, LastExitAt: base, LastExitFailed: true, CrashCount: 1}}},
			steps:  []string{"start 1"}},
		{name: "25b ... and waits out its backoff", spec: s10,
			copies: []CopyView{{App: state.AppState{Name: "app", PID: 77, Port: 8080, StartedAt: base, LastExitAt: now.Add(-time.Second), LastExitFailed: true, CrashCount: 2}}},
			steps:  []string{"backoff 1"}},
		{name: "29 scale down in a rollout keeps the copy on the new version", spec: s10, revision: 1,
			copies: []CopyView{ready(s10, 1, 0), withRevision(ready(s10, 2, 10*time.Second), 1)},
			steps:  []string{"stop 1 pid=101"}, active: []int{2}},
		{name: "30 ... whatever the old copy holds", spec: s10, revision: 1,
			copies: []CopyView{withSessions(ready(s10, 1, 0), 4), withRevision(ready(s10, 2, 10*time.Second), 1)},
			steps:  []string{"stop 1 pid=101"}, active: []int{2}},
		{name: "31 scale down with a starting copy and a ready one on the new version", spec: s10, revision: 1,
			copies: []CopyView{ready(s10, 1, 0), withRevision(startingCopy(s10, 2, 20*time.Second), 1), withRevision(ready(s10, 3, 10*time.Second), 1)},
			steps:  []string{"stop 1 pid=101", "stop 2 pid=102"}, active: []int{3}},
		{name: "32 the rollout waits for a copy that is yet to be started", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0)},
			steps:  []string{"start 2"}, active: []int{1}, rolling: true},
		{name: "33 scale down while the new copy is still starting: the old one keeps serving", spec: s10, revision: 1,
			copies: []CopyView{ready(s10, 1, 0), withRevision(startingCopy(s10, 2, 10*time.Second), 1)},
			active: []int{1}, rolling: true},
		{name: "34 ... and goes once the new one is ready", spec: s10, revision: 1,
			copies: []CopyView{ready(s10, 1, 0), withRevision(ready(s10, 2, 10*time.Second), 1)},
			steps:  []string{"stop 1 pid=101"}, active: []int{2}},
		{name: "35 two old ready copies and a starting new one, one wanted: only the active one stays", spec: s10, revision: 1,
			copies: []CopyView{ready(s10, 1, 0), ready(s10, 2, time.Second), withRevision(startingCopy(s10, 3, 10*time.Second), 1)},
			steps:  []string{"stop 2 pid=102"}, active: []int{1}, rolling: true},
		{name: "36 two wanted, two new ones starting: both old ones stay, the active one first", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(startingCopy(s11, 3, 10*time.Second), 1), withRevision(startingCopy(s11, 4, 11*time.Second), 1)},
			active: []int{1}, warm: []int{2}, rolling: true},
		{name: "36b two wanted, three old ones ready, two new ones starting: two old ones stay", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), ready(s11, 3, 2*time.Second), withRevision(startingCopy(s11, 4, 10*time.Second), 1), withRevision(startingCopy(s11, 5, 11*time.Second), 1)},
			steps:  []string{"stop 3 pid=103"}, active: []int{1}, warm: []int{2}, rolling: true},
		{name: "37 scale down, the new copy crashed: it is retried, the old one keeps serving", spec: s10, revision: 1,
			copies: []CopyView{ready(s10, 1, 0), withRevision(dead(s10, 2, exited(now.Add(-time.Second), 1)), 1)},
			steps:  []string{"backoff 2"}, active: []int{1}, rolling: true},
		{name: "38 ... after its backoff it is started again", spec: s10, revision: 1,
			copies: []CopyView{ready(s10, 1, 0), withRevision(dead(s10, 2, exited(base, 1)), 1)},
			steps:  []string{"start 2"}, active: []int{1}, rolling: true},
		{name: "39 ... and when it gives up the old one still serves", spec: withPolicy(s10, manifest.RestartNever), revision: 1,
			copies: []CopyView{ready(s10, 1, 0), withRevision(dead(s10, 2, exited(base, 1)), 1)},
			active: []int{1}, rolling: true},
		{name: "40 two wanted, a new copy ready and another in backoff: the dead one goes and the rollout goes on (round 3 stopped both old ones and kept the dead one)", spec: copySpec(2, 0), revision: 1,
			copies: []CopyView{ready(copySpec(2, 0), 1, 0), ready(copySpec(2, 0), 2, time.Second), withRevision(ready(copySpec(2, 0), 3, 10*time.Second), 1),
				withRevision(dead(copySpec(2, 0), 4, exited(now.Add(-time.Second), 1)), 1)},
			steps: []string{"stop 2 pid=102", "stop 4", "start 1 pid=101 replace restart"}, active: []int{3}},
		{name: "27 replicas 0 means 1", spec: manifest.AppSpec{AppName: "app", EntrypointFile: "/srv/app.R", WarmReplicas: intPtr(0)},
			steps: []string{"start 1"}},
		{name: "28 no warm-replicas key means one spare", spec: manifest.AppSpec{AppName: "app", EntrypointFile: "/srv/app.R"},
			steps: []string{"start 1", "start 2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideCopies(tc.spec, tc.revision, tc.copies, now)
			if want := tc.steps; !reflect.DeepEqual(show(got.Steps), want) && !(len(want) == 0 && len(got.Steps) == 0) {
				t.Errorf("steps = %q, want %q", show(got.Steps), want)
			}
			if !sameInts(got.Active, tc.active) || !sameInts(got.Warm, tc.warm) {
				t.Errorf("active %v warm %v, want active %v warm %v", got.Active, got.Warm, tc.active, tc.warm)
			}
			if got.Rolling != tc.rolling {
				t.Errorf("Rolling = %v, want %v", got.Rolling, tc.rolling)
			}
		})
	}
}

func withEnv(s manifest.AppSpec, k, v string) manifest.AppSpec {
	s.Env = map[string]string{k: v}
	return s
}

func sameInts(a, b []int) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// A stalled rollout names the copies it waits for.
func TestDecideCopies_WaitingAndOutdated(t *testing.T) {
	s := copySpec(1, 1)
	p := DecideCopies(s, 1, []CopyView{ready(s, 1, 0), withRevision(startingCopy(s, 2, time.Second), 1)}, now)
	if !reflect.DeepEqual(p.Waiting, []int{2}) || !reflect.DeepEqual(p.Outdated, []int{1}) {
		t.Errorf("Waiting %v Outdated %v, want [2] and [1]", p.Waiting, p.Outdated)
	}
}

// With three outdated copies and none current, one is replaced; the next
// reconcile waits for it.
func TestDecideCopies_OneAtATime(t *testing.T) {
	s := copySpec(3, 0)
	views := []CopyView{ready(s, 1, 0), ready(s, 2, time.Second), ready(s, 3, 2*time.Second)}
	p := DecideCopies(s, 1, views, now)
	if got := show(p.Steps); len(got) != 1 {
		t.Fatalf("steps = %q, want one replacement", got)
	}

	// The replaced copy is back, current, but not ready yet.
	views[2] = withRevision(startingCopy(s, 3, 10*time.Second), 1)
	p = DecideCopies(s, 1, views, now)
	if len(p.Steps) != 0 || !p.Rolling {
		t.Errorf("while the replacement starts: steps %q rolling %v, want a wait", show(p.Steps), p.Rolling)
	}

	// Once it is ready the next one goes.
	views[2] = withRevision(ready(s, 3, 10*time.Second), 1)
	p = DecideCopies(s, 1, views, now)
	if got := show(p.Steps); len(got) != 1 || !strings.HasPrefix(got[0], "start ") || !strings.Contains(got[0], "replace") {
		t.Errorf("steps = %q, want the next replacement", got)
	}
}

func TestPlan_Action(t *testing.T) {
	cases := []struct {
		steps []Step
		want  ActionType
	}{
		{nil, ActionNone},
		{[]Step{{Type: ActionStop}}, ActionNone},
		{[]Step{{Type: ActionBackoff}}, ActionBackoff},
		{[]Step{{Type: ActionBackoff}, {Type: ActionStop}}, ActionNone},
		{[]Step{{Type: ActionBackoff}, {Type: ActionStart}}, ActionStart},
		{[]Step{{Type: ActionStop}, {Type: ActionStart}}, ActionStart},
	}
	for _, tc := range cases {
		if got := (Plan{Steps: tc.steps}).Action(); got != tc.want {
			t.Errorf("%v: Action = %q, want %q", show(tc.steps), got, tc.want)
		}
	}
}

func TestRoles(t *testing.T) {
	s := copySpec(1, 1)
	cases := []struct {
		name         string
		views        []CopyView
		active, warm []int
	}{
		{"3", []CopyView{ready(s, 1, 0), ready(s, 2, time.Second)}, []int{1}, []int{2}},
		{"4", []CopyView{ready(s, 1, time.Minute), ready(s, 2, time.Second)}, []int{2}, []int{1}},
		{"5", []CopyView{dead(s, 1, nil), ready(s, 2, time.Second)}, []int{2}, nil},
		{"10", []CopyView{ready(s, 1, 0), startingCopy(s, 2, time.Second)}, []int{1}, nil},
		{"none", nil, nil, nil},
	}
	for _, tc := range cases {
		active, warm := Roles(s, tc.views)
		if !sameInts(active, tc.active) || !sameInts(warm, tc.warm) {
			t.Errorf("%s: active %v warm %v, want %v and %v", tc.name, active, warm, tc.active, tc.warm)
		}
		p := DecideCopies(s, 0, tc.views, now)
		if tc.name != "none" && tc.name != "5" && (!sameInts(p.Active, active) || !sameInts(p.Warm, warm)) {
			t.Errorf("%s: DecideCopies roles %v %v differ from Roles %v %v", tc.name, p.Active, p.Warm, active, warm)
		}
	}
}

func TestOutdated(t *testing.T) {
	s := withEnv(copySpec(1, 1), "A", "1")
	a := rec(s, 1, 0, 2)

	if changed, restart := Outdated(s, 2, a); len(changed) != 0 || restart {
		t.Errorf("same version: %v %v", changed, restart)
	}
	if changed, restart := Outdated(s, 3, a); len(changed) != 0 || !restart {
		t.Errorf("revision behind: %v %v, want restart", changed, restart)
	}
	if changed, _ := Outdated(withEnv(s, "A", "2"), 2, a); !reflect.DeepEqual(changed, []string{"env"}) {
		t.Errorf("env changed: %v", changed)
	}

	// A record without started-from only reports the name and the revision.
	old := a
	old.StartedFrom = nil
	changedSpec := withEnv(s, "A", "2")
	changedSpec.EntrypointFile = "/srv/other.R"
	if changed, _ := Outdated(changedSpec, 2, old); len(changed) != 0 {
		t.Errorf("a record without started-from: %v, want nothing", changed)
	}
	changedSpec.AppName = "renamed"
	if changed, _ := Outdated(changedSpec, 2, old); !reflect.DeepEqual(changed, []string{"app-name"}) {
		t.Errorf("a renamed app: %v", changed)
	}
}

func TestDescribe(t *testing.T) {
	s11, s10, s20, s21 := copySpec(1, 1), copySpec(1, 0), copySpec(2, 0), copySpec(2, 1)
	step := func(typ ActionType, copy int) Step { return Step{Type: typ, Copy: copy} }

	cases := []struct {
		name     string
		spec     manifest.AppSpec
		before   []CopyView
		plan     Plan
		settings []string
		want     string
	}{
		{"fresh, one copy", s10, nil, Plan{Steps: []Step{step(ActionStart, 1)}}, nil,
			`Starting app "app" on port 8080`},
		{"fresh, a spare", s11, nil, Plan{Steps: []Step{step(ActionStart, 1), step(ActionStart, 2)}}, nil,
			`Starting app "app" on port 8080 (1 copy and 1 warm spare)`},
		{"fresh, several", copySpec(2, 3), nil, Plan{Steps: []Step{step(ActionStart, 1)}}, nil,
			`Starting app "app" on port 8080 (2 copies and 3 warm spares)`},
		{"fresh, copies only", s20, nil, Plan{Steps: []Step{step(ActionStart, 1)}}, nil,
			`Starting app "app" on port 8080 (2 copies)`},
		{"after a crash, with a setting", s10, []CopyView{dead(s10, 1, exited(base, 1))}, Plan{Steps: []Step{step(ActionStart, 1)}}, []string{"port"},
			`Starting app "app" on port 8080 (manifest changed: port)`},
		{"upgrade, one copy", s10, nil,
			Plan{Steps: []Step{{Type: ActionStop, Copy: 0, PID: 123}, {Type: ActionStart, Copy: 1, Upgrade: true}}}, nil,
			`Starting app "app" on port 8080 (moved behind the load balancer; stopped the previous instance: pid 123)`},
		{"upgrade, a spare", s11, nil,
			Plan{Steps: []Step{{Type: ActionStop, Copy: 0, PID: 123}, {Type: ActionStart, Copy: 1, Upgrade: true}, step(ActionStart, 2)}}, nil,
			`Starting app "app" on port 8080 (1 copy and 1 warm spare; moved behind the load balancer; stopped the previous instance: pid 123)`},
		{"replace, nothing else alive, manifest", s10, []CopyView{ready(s10, 1, 0)},
			Plan{Steps: []Step{{Type: ActionStart, Copy: 1, PID: 123, Replace: true, Changed: []string{"env"}}}}, nil,
			`Starting app "app" on port 8080 (manifest changed: env; stopped the previous instance: pid 123)`},
		{"replace, a restart", s10, []CopyView{ready(s10, 1, 0)},
			Plan{Steps: []Step{{Type: ActionStart, Copy: 1, PID: 123, Replace: true, Restart: true}}}, nil,
			`Starting app "app" on port 8080 (stopped the previous instance: pid 123)`},
		{"replace, with a setting", s10, []CopyView{ready(s10, 1, 0)},
			Plan{Steps: []Step{{Type: ActionStart, Copy: 1, PID: 123, Replace: true, Changed: []string{"env"}}}}, []string{"port"},
			`Starting app "app" on port 8080 (manifest changed: port, env; stopped the previous instance: pid 123)`},
		{"replace, another copy alive, manifest", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			Plan{Steps: []Step{{Type: ActionStart, Copy: 2, PID: 124, Replace: true, Changed: []string{"env"}}}, Outdated: []int{1, 2}}, nil,
			`Restarting app "app" one copy at a time (manifest changed: env): copy 2 first (stopped pid 124)`},
		{"replace, another copy alive, restart", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			Plan{Steps: []Step{{Type: ActionStart, Copy: 2, PID: 124, Replace: true, Restart: true}}, Outdated: []int{1, 2}}, nil,
			`Restarting app "app" one copy at a time: copy 2 first (stopped pid 124)`},
		{"replace, the second one", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			Plan{Steps: []Step{{Type: ActionStart, Copy: 1, PID: 123, Replace: true, Restart: true}}, Outdated: []int{1}}, nil,
			`Restarting app "app" one copy at a time: copy 1 next (stopped pid 123)`},
		{"replace with a scale-down", s10, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), ready(s11, 3, 2*time.Second)},
			Plan{Steps: []Step{{Type: ActionStop, Copy: 3, PID: 103}, {Type: ActionStart, Copy: 2, PID: 102, Replace: true, Restart: true}}, Outdated: []int{1, 2, 3}}, []string{"warm-replicas"},
			`Restarting app "app" one copy at a time (manifest changed: warm-replicas): copy 2 first (stopped pid 102); stopping copy 3`},
		{"replace alone with a scale-down", s10, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			Plan{Steps: []Step{{Type: ActionStop, Copy: 2, PID: 102}, {Type: ActionStart, Copy: 1, PID: 101, Replace: true, Restart: true}}, Outdated: []int{1}}, nil,
			`Starting app "app" on port 8080 (stopping copy 2; stopped the previous instance: pid 101)`},
		{"waiting, with a setting", s10, []CopyView{ready(s10, 1, 0), startingCopy(s10, 2, time.Second)},
			Plan{Rolling: true, Waiting: []int{2}, Outdated: []int{1}}, []string{"warm-replicas"},
			`Restarting app "app" one copy at a time (manifest changed: warm-replicas): waiting for copy 2 to be ready`},
		{"waiting", s11, []CopyView{ready(s11, 1, 0), startingCopy(s11, 2, time.Second)},
			Plan{Rolling: true, Waiting: []int{2}, Outdated: []int{1}}, nil,
			`Restarting app "app" one copy at a time: waiting for copy 2 to be ready`},
		{"waiting for several", s21, nil,
			Plan{Rolling: true, Waiting: []int{2, 3}}, nil,
			`Restarting app "app" one copy at a time: waiting for copies 2 and 3 to be ready`},
		{"scale up", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			Plan{Steps: []Step{step(ActionStart, 3)}}, []string{"replicas"},
			`Updated "app" without a restart (manifest changed: replicas; starting copy 3)`},
		{"scale up by two", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			Plan{Steps: []Step{step(ActionStart, 3), step(ActionStart, 4)}}, []string{"replicas"},
			`Updated "app" without a restart (manifest changed: replicas; starting copies 3 and 4)`},
		{"scale down", s10, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			Plan{Steps: []Step{{Type: ActionStop, Copy: 2, PID: 102}}}, []string{"warm-replicas"},
			`Updated "app" without a restart (manifest changed: warm-replicas; stopping copy 2)`},
		{"scale down by three", s10, []CopyView{ready(s11, 1, 0)},
			Plan{Steps: []Step{step(ActionStop, 2), step(ActionStop, 4), step(ActionStop, 5)}}, []string{"replicas"},
			`Updated "app" without a restart (manifest changed: replicas; stopping copies 2, 4 and 5)`},
		{"lb-policy", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)}, Plan{}, []string{"lb-policy"},
			`Updated "app" without a restart (manifest changed: lb-policy)`},
		{"port and host", s10, []CopyView{ready(s10, 1, 0)}, Plan{}, []string{"port", "host"},
			`Updated "app" without a restart (manifest changed: port, host)`},
		{"a dead copy started again", s11, []CopyView{ready(s11, 1, 0), dead(s11, 2, exited(base, 1))},
			Plan{Steps: []Step{step(ActionStart, 2)}}, nil,
			`Updated "app" (starting copy 2 again)`},
		{"nothing", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)}, Plan{}, nil,
			`Nothing to change`},
		{"backoff, one copy", s10, []CopyView{dead(s10, 1, exited(base, 3))}, Plan{Steps: []Step{step(ActionBackoff, 1)}}, nil,
			`Backing off restart of "app" after 3 crash(es)`},
		{"backoff, a copy", s11, []CopyView{ready(s11, 1, 0), dead(s11, 2, exited(base, 3))}, Plan{Steps: []Step{step(ActionBackoff, 2)}}, nil,
			`Backing off restart of "app" copy 2 after 3 crash(es)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Describe(tc.spec, tc.before, tc.plan, tc.settings); got != tc.want {
				t.Errorf("Describe =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

func TestCopiesSummary(t *testing.T) {
	for _, tc := range []struct {
		r, w int
		want string
	}{
		{1, 0, "1 copy"}, {2, 0, "2 copies"}, {1, 1, "1 copy and 1 warm spare"}, {2, 3, "2 copies and 3 warm spares"},
	} {
		if got := copiesSummary(tc.r, tc.w); got != tc.want {
			t.Errorf("copiesSummary(%d, %d) = %q, want %q", tc.r, tc.w, got, tc.want)
		}
	}
}

func TestSettingsChanged_Copies(t *testing.T) {
	spec := func(r int, w *int, lb string) manifest.AppSpec {
		return manifest.AppSpec{Port: 8080, Replicas: r, WarmReplicas: w, LBPolicy: lb}
	}
	cases := []struct {
		name      string
		prev, cur manifest.AppSpec
		want      []string
	}{
		{"replicas 0 and 1 are the same", spec(0, nil, ""), spec(1, nil, ""), nil},
		{"replicas 1 to 2", spec(1, nil, ""), spec(2, nil, ""), []string{"replicas"}},
		{"unset warm-replicas is 1", spec(1, nil, ""), spec(1, intPtr(1), ""), nil},
		{"warm-replicas to 0", spec(1, nil, ""), spec(1, intPtr(0), ""), []string{"warm-replicas"}},
		{"unset lb-policy is least-conn", spec(1, nil, ""), spec(1, nil, "least-conn"), nil},
		{"lb-policy to first", spec(1, nil, "least-conn"), spec(1, nil, "first"), []string{"lb-policy"}},
		{"all in the manifest's order", manifest.AppSpec{Port: 1, Replicas: 1, RestartPolicy: manifest.RestartAlways},
			manifest.AppSpec{Port: 2, Host: "127.0.0.1", Replicas: 2, WarmReplicas: intPtr(0), LBPolicy: "first", RestartPolicy: manifest.RestartNever},
			[]string{"port", "host", "replicas", "warm-replicas", "lb-policy", "restart-policy"}},
	}
	for _, tc := range cases {
		if got := SettingsChanged(tc.prev, tc.cur); !sameStrings(got, tc.want) {
			t.Errorf("%s: SettingsChanged = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func sameStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestDecideCopies_NewVersionWaitsForFreshSlots(t *testing.T) {
	s := copySpec(1, 1)
	p := DecideCopies(s, 1, []CopyView{ready(s, 1, 0)}, now)
	if !reflect.DeepEqual(p.Waiting, []int{2}) || !p.Rolling {
		t.Errorf("Waiting %v Rolling %v, want [2] and true", p.Waiting, p.Rolling)
	}
}

// Properties of every plan, over random views:
//   - P1: no empty Active set while a ready copy that runs the current version
//     exists;
//   - P2: none while two or more copies are ready and two or more are wanted;
//   - P3: at most one copy is replaced.
//
// (An empty Active set is left only where it is meant: a lone old copy with
// one copy wanted, and an upgrade from a process of an older build.) The
// views include copies that are pending, starting, in backoff and given up.
func TestDecideCopies_ActiveSetProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 100000; i++ {
		replicas, warm := 1+rng.Intn(3), rng.Intn(3)
		s := copySpec(replicas, warm)
		revision := rng.Intn(2)
		var views []CopyView
		for n := 1; n <= 1+rng.Intn(6); n++ {
			v := CopyView{App: rec(s, n, time.Duration(rng.Intn(60))*time.Second, rng.Intn(2)), Sessions: rng.Intn(3)}
			switch rng.Intn(6) {
			case 0:
			case 1:
				v.Pending = true
			case 2:
				v.Alive = true
			default:
				v.Alive, v.Ready = true, true
			}
			if !v.Alive && !v.Pending && rng.Intn(2) == 0 {
				v.App.LastExitAt, v.App.LastExitFailed, v.App.CrashCount = now.Add(-time.Second), true, 1+rng.Intn(3)
			}
			views = append(views, v)
		}
		p := DecideCopies(s, revision, views, now)

		currentReady, ready := 0, 0
		for _, v := range views {
			if v.Alive && v.Ready {
				ready++
				if ch, r := Outdated(s, revision, v.App); len(ch) == 0 && !r {
					currentReady++
				}
			}
		}
		if len(p.Active) == 0 && currentReady > 0 {
			t.Fatalf("P1: %d current ready copies, Active empty: spec %d+%d rev %d views %+v steps %v", currentReady, replicas, warm, revision, views, show(p.Steps))
		}
		if len(p.Active) == 0 && ready >= 2 && s.CopyCount() >= 2 {
			t.Fatalf("P2: %d ready copies, %d wanted, Active empty: spec %d+%d rev %d views %+v steps %v", ready, s.CopyCount(), replicas, warm, revision, views, show(p.Steps))
		}
		replaced := 0
		for _, st := range p.Steps {
			if st.Replace {
				replaced++
			}
		}
		if replaced > 1 {
			t.Fatalf("P3: %d replacements: %v", replaced, show(p.Steps))
		}
	}
}
