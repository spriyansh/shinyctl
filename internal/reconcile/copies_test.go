package reconcile

import (
	"fmt"
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

func withDrain(s manifest.AppSpec, d string) manifest.AppSpec { s.DrainTimeout = d; return s }

// rec is the record of copy n started age after base, from spec, at
// revision rev.
func rec(spec manifest.AppSpec, n int, age time.Duration, rev int) state.AppState {
	return state.AppState{
		Name: spec.AppName, PID: 100 + n, Copy: n, Port: 20000 + n,
		EntrypointFile: spec.EntrypointFile, StartedAt: base.Add(age),
		StartedFrom: spec.StartFields(), Revision: rev,
	}
}

// ready is a copy that is alive and accepting connections, with no sessions
// by the load balancer's count.
func ready(spec manifest.AppSpec, n int, age time.Duration) CopyView {
	return CopyView{App: rec(spec, n, age, 0), Alive: true, Ready: true, SessionsKnown: true}
}

func startingCopy(spec manifest.AppSpec, n int, age time.Duration) CopyView {
	return CopyView{App: rec(spec, n, age, 0), Alive: true, SessionsKnown: true}
}

// dead is a copy whose process is gone; mutate sets its crash history.
func dead(spec manifest.AppSpec, n int, mutate func(*state.AppState)) CopyView {
	a := rec(spec, n, 0, 0)
	if mutate != nil {
		mutate(&a)
	}
	return CopyView{App: a, SessionsKnown: true}
}

func exited(at time.Time, crashes int) func(*state.AppState) {
	return func(a *state.AppState) {
		a.LastExitAt, a.LastExitFailed, a.CrashCount = at, true, crashes
	}
}

func withRevision(v CopyView, rev int) CopyView { v.App.Revision = rev; return v }
func withSessions(v CopyView, n int) CopyView   { v.Sessions = n; return v }
func unknownSessions(v CopyView) CopyView       { v.Sessions, v.SessionsKnown = 0, false; return v }
func pending(v CopyView) CopyView               { v.Alive, v.Ready, v.Pending = false, false, true; return v }
func drainingSince(v CopyView, ago time.Duration) CopyView {
	v.App.DrainingSince = now.Add(-ago)
	return v
}
func skip(v CopyView) CopyView { v.App.SkipDrain = true; return v }
func withPolicy(s manifest.AppSpec, p manifest.RestartPolicy) manifest.AppSpec {
	s.RestartPolicy = p
	return s
}
func withEnv(s manifest.AppSpec, k, v string) manifest.AppSpec {
	s.Env = map[string]string{k: v}
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
		if s.Type == ActionStop && s.Why != "" {
			fmt.Fprintf(&b, " %s", s.Why)
		}
		if s.Upgrade {
			b.WriteString(" upgrade")
		}
		out = append(out, b.String())
	}
	return out
}

func sameInts(a, b []int) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func TestDecideCopies(t *testing.T) {
	s11, s20, s21, s31, s10 := copySpec(1, 1), copySpec(2, 0), copySpec(2, 1), copySpec(3, 1), copySpec(1, 0)
	envChanged := withEnv(s10, "B", "2")

	cases := []struct {
		name      string
		spec      manifest.AppSpec
		revision  int
		skipDrain bool
		copies    []CopyView
		steps     []string
		drain     []int
		active    []int
		warm      []int
		draining  []int
		rolling   bool
		switched  bool
	}{
		// Unchanged from the builds that replaced copies in place.
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
		{name: "8 dead and given up (never): nothing", spec: withPolicy(s11, manifest.RestartNever),
			copies: []CopyView{dead(s11, 1, exited(base, 1)), ready(s11, 2, time.Second)},
			active: []int{2}},
		{name: "9 everything dead and given up: nothing to do", spec: withPolicy(s11, manifest.RestartNever),
			copies: []CopyView{dead(s11, 1, exited(base, 1)), dead(s11, 2, exited(base, 1))}},
		{name: "10 a copy that is not ready has no role", spec: s11,
			copies: []CopyView{ready(s11, 1, 0), startingCopy(s11, 2, time.Second)},
			active: []int{1}},
		{name: "11 scale up", spec: s21,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			steps:  []string{"start 3"}, active: []int{1, 2}},
		{name: "14 scale down: a copy with no process goes first, its record only", spec: s11,
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), dead(s21, 3, exited(base, 1))},
			steps:  []string{"stop 3 surplus"}, active: []int{1}, warm: []int{2}},
		{name: "15 scale down: a starting copy is stopped, not drained", spec: s10,
			copies: []CopyView{ready(s11, 1, 0), startingCopy(s11, 2, time.Second)},
			steps:  []string{"stop 2 pid=102 surplus"}, active: []int{1}},
		{name: "16 sparse numbers take the lowest free one", spec: copySpec(2, 1),
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 3, time.Second)},
			steps:  []string{"start 2"}, active: []int{1, 3}},
		{name: "23 outdated and dead: a plain start", spec: s10, revision: 1,
			copies: []CopyView{dead(s10, 1, exited(base, 1))},
			steps:  []string{"start 1"}},
		{name: "24 a process from before the load balancer", spec: s11,
			copies: []CopyView{{App: state.AppState{Name: "app", PID: 77, Port: 8080, EntrypointFile: s11.EntrypointFile, StartedAt: base}, Alive: true, Ready: true}},
			steps:  []string{"stop 0 pid=77 upgrade", "start 1 upgrade", "start 2"}},
		{name: "25a dead record from before the load balancer is copy 1", spec: s10,
			copies: []CopyView{{App: state.AppState{Name: "app", PID: 77, Port: 8080, StartedAt: base, LastExitAt: base, LastExitFailed: true, CrashCount: 1}}},
			steps:  []string{"start 1"}},
		{name: "25b ... and waits out its backoff", spec: s10,
			copies: []CopyView{{App: state.AppState{Name: "app", PID: 77, Port: 8080, StartedAt: base, LastExitAt: now.Add(-time.Second), LastExitFailed: true, CrashCount: 2}}},
			steps:  []string{"backoff 1"}},
		{name: "27 replicas 0 means 1", spec: manifest.AppSpec{AppName: "app", EntrypointFile: "/srv/app.R", WarmReplicas: intPtr(0)},
			steps: []string{"start 1"}},
		{name: "28 no warm-replicas key means one spare", spec: manifest.AppSpec{AppName: "app", EntrypointFile: "/srv/app.R"},
			steps: []string{"start 1", "start 2"}},

		// Scale-down drains (builds that replaced copies in place stopped these copies).
		{name: "12 scale down: the spare drains", spec: s11,
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second)},
			drain:  []int{3}, active: []int{1}, warm: []int{2}, draining: []int{3}},
		{name: "13 scale down: the spare, then the active with the fewest sessions", spec: s11,
			copies: []CopyView{
				withSessions(ready(s31, 1, 0), 5), withSessions(ready(s31, 2, time.Second), 0),
				withSessions(ready(s31, 3, 2*time.Second), 2), ready(s31, 4, 3*time.Second),
			},
			drain: []int{2, 4}, active: []int{1}, warm: []int{3}, draining: []int{2, 4}},
		{name: "12n scale down with --now: stopped at once", spec: s11, skipDrain: true,
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second)},
			steps:  []string{"stop 3 pid=103 surplus"}, active: []int{1}, warm: []int{2}},
		{name: "12z scale down with drain-timeout 0: stopped at once", spec: withDrain(s11, "0"),
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second)},
			steps:  []string{"stop 3 pid=103 surplus"}, active: []int{1}, warm: []int{2}},

		// A rollout.
		{name: "17 env changed, one copy: a new copy starts, the old one serves", spec: envChanged,
			copies: []CopyView{ready(s10, 1, 0)},
			steps:  []string{"start 2"}, active: []int{1}, rolling: true},
		{name: "18 restart, a copy and a spare: only the serving copy starts, the spare comes after the switch", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			steps:  []string{"start 3"}, active: []int{1}, warm: []int{2}, rolling: true},
		{name: "18b restart, two copies and a spare: two new copies start", spec: s21, revision: 1,
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second)},
			steps:  []string{"start 4", "start 5"}, active: []int{1, 2}, warm: []int{3}, rolling: true},
		{name: "18c ... one of them ready is not the switch", spec: s21, revision: 1,
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second), withRevision(ready(s21, 4, time.Minute), 1), withRevision(startingCopy(s21, 5, time.Minute), 1)},
			active: []int{1, 2}, warm: []int{3, 4}, rolling: true},
		{name: "18d ... both ready: the switch, the old spare stops, the new spare starts", spec: s21, revision: 1,
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second), withRevision(ready(s21, 4, time.Minute), 1), withRevision(ready(s21, 5, time.Minute), 1)},
			steps:  []string{"stop 3 pid=103 idle", "start 6"}, drain: []int{1, 2}, active: []int{4, 5}, draining: []int{1, 2}, switched: true},
		{name: "18e a restart with no spares: one new copy", spec: s10, revision: 1,
			copies: []CopyView{ready(s10, 1, 0)},
			steps:  []string{"start 2"}, active: []int{1}, rolling: true},
		{name: "19 the new copy is starting: the old ones keep their roles", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(startingCopy(s11, 3, time.Minute), 1)},
			active: []int{1}, warm: []int{2}, rolling: true},
		{name: "19a ... also with a spare of the new version already there", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(startingCopy(s11, 3, time.Minute), 1), withRevision(startingCopy(s11, 4, time.Minute), 1)},
			active: []int{1}, warm: []int{2}, rolling: true},
		{name: "19b replicas new copies ready: the switch; the old active copy drains, the old spare stops, the new spare starts", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(ready(s11, 3, time.Minute), 1)},
			steps:  []string{"stop 2 pid=102 idle", "start 4"}, drain: []int{1}, active: []int{3}, draining: []int{1}, switched: true},
		{name: "19b2 ... an old spare that holds visitors (it stood in) drains too", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), withSessions(ready(s11, 2, time.Second), 1), withRevision(ready(s11, 3, time.Minute), 1)},
			steps:  []string{"start 4"}, drain: []int{1, 2}, active: []int{3}, draining: []int{1, 2}, switched: true},
		{name: "19b3 ... or whose sessions are not known", spec: s11, revision: 1,
			copies: []CopyView{unknownSessions(ready(s11, 1, 0)), unknownSessions(ready(s11, 2, time.Second)), withRevision(ready(s11, 3, time.Minute), 1)},
			steps:  []string{"start 4"}, drain: []int{1, 2}, active: []int{3}, draining: []int{1, 2}, switched: true},
		{name: "19b4 ... the new spare already there: nothing starts", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(ready(s11, 3, time.Minute), 1), withRevision(startingCopy(s11, 4, time.Minute), 1)},
			steps:  []string{"stop 2 pid=102 idle"}, drain: []int{1}, active: []int{3}, draining: []int{1}, switched: true},
		{name: "19c fewer than replicas ready: no switch, the new one is a spare", spec: s20, revision: 1,
			copies: []CopyView{ready(s20, 1, 0), ready(s20, 2, time.Second), withRevision(ready(s20, 3, time.Minute), 1), withRevision(startingCopy(s20, 4, time.Minute), 1)},
			active: []int{1, 2}, warm: []int{3}, rolling: true},
		{name: "19d an old copy is missing: a new ready copy fills its place", spec: s20, revision: 1,
			copies: []CopyView{ready(s20, 1, 0), dead(s20, 2, exited(now.Add(-time.Second), 1)), withRevision(ready(s20, 3, time.Minute), 1), withRevision(startingCopy(s20, 4, time.Minute), 1)},
			steps:  []string{"stop 2 surplus"}, active: []int{1, 3}, rolling: true},
		{name: "20 the new version crashes: retried, the old copies serve", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(dead(s11, 3, exited(now.Add(-time.Second), 2)), 1), withRevision(startingCopy(s11, 4, time.Minute), 1)},
			steps:  []string{"backoff 3"}, active: []int{1}, warm: []int{2}, rolling: true},
		{name: "21 the new version gave up: the old copies serve", spec: withPolicy(s11, manifest.RestartNever), revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(dead(s11, 3, exited(base, 1)), 1), withRevision(dead(s11, 4, exited(base, 1)), 1)},
			active: []int{1}, warm: []int{2}, rolling: true},
		{name: "22 an old copy that is not ready and has no sessions stops", spec: s10, revision: 1,
			copies: []CopyView{ready(s10, 1, 0), startingCopy(s10, 2, time.Second)},
			steps:  []string{"stop 2 pid=102 outdated", "start 3"}, active: []int{1}, rolling: true},
		{name: "22b ... but not while the load balancer can't say", spec: s10, revision: 1,
			copies: []CopyView{ready(s10, 1, 0), unknownSessions(startingCopy(s10, 2, time.Second))},
			steps:  []string{"start 3"}, active: []int{1}, rolling: true},
		{name: "26 a restart during a rollout: the new copies not ready yet go, one more starts", spec: s11, revision: 2,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(startingCopy(s11, 3, time.Minute), 1), withRevision(startingCopy(s11, 4, time.Minute), 1)},
			steps:  []string{"stop 3 pid=103 outdated", "stop 4 pid=104 outdated", "start 5"}, active: []int{1}, warm: []int{2}, rolling: true},
		{name: "27b a restart after the switch: a second version drains", spec: s11, revision: 2,
			copies: []CopyView{withSessions(drainingSince(ready(s11, 1, 0), time.Minute), 2), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute+time.Second), 1)},
			steps:  []string{"start 2"}, active: []int{3}, warm: []int{4}, draining: []int{1}, rolling: true},
		{name: "27c ... and once the newest is ready both drain, longest first", spec: s11, revision: 2,
			copies: []CopyView{withSessions(drainingSince(ready(s11, 1, 0), time.Minute), 2), withRevision(ready(s11, 2, 2*time.Minute), 2), withRevision(ready(s11, 3, time.Minute), 1), withRevision(withSessions(ready(s11, 4, time.Minute+time.Second), 1), 1), withRevision(startingCopy(s11, 5, 2*time.Minute), 2)},
			drain:  []int{3, 4}, active: []int{2}, draining: []int{1, 3, 4}, switched: true},

		// Draining copies.
		{name: "28b no sessions left after the settle: stopped", spec: s11, revision: 1,
			copies: []CopyView{drainingSince(ready(s11, 1, 0), 11*time.Second), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute), 1)},
			steps:  []string{"stop 1 pid=101 drained"}, active: []int{3}, warm: []int{4}},
		{name: "28c ... not before the settle", spec: s11, revision: 1,
			copies: []CopyView{drainingSince(ready(s11, 1, 0), 5*time.Second), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute), 1)},
			active: []int{3}, warm: []int{4}, draining: []int{1}},
		{name: "28d ... nor when the load balancer can't say", spec: s11, revision: 1,
			copies: []CopyView{unknownSessions(drainingSince(ready(s11, 1, 0), time.Minute)), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute), 1)},
			active: []int{3}, warm: []int{4}, draining: []int{1}},
		{name: "28e drain-timeout passed: stopped with its sessions", spec: s11, revision: 1,
			copies: []CopyView{withSessions(drainingSince(ready(s11, 1, 0), 15*time.Minute), 3), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute), 1)},
			steps:  []string{"stop 1 pid=101 timeout"}, active: []int{3}, warm: []int{4}},
		{name: "28f a shorter drain-timeout applies to drains already going", spec: withDrain(s11, "5m"), revision: 1,
			copies: []CopyView{withSessions(drainingSince(ready(s11, 1, 0), 6*time.Minute), 3), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute), 1)},
			steps:  []string{"stop 1 pid=101 timeout"}, active: []int{3}, warm: []int{4}},
		{name: "28g --now ends every drain", spec: s11, revision: 1, skipDrain: true,
			copies: []CopyView{withSessions(drainingSince(ready(s11, 1, 0), time.Minute), 3), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute), 1)},
			steps:  []string{"stop 1 pid=101 no-drain"}, active: []int{3}, warm: []int{4}},
		{name: "28h a copy marked to skip the drain stops at the switch", spec: s10, revision: 1,
			copies: []CopyView{withSessions(skip(ready(s10, 1, 0)), 2), withRevision(ready(s10, 3, time.Minute), 1)},
			steps:  []string{"stop 1 pid=101 no-drain"}, active: []int{3}, switched: true},
		{name: "28i drain-timeout 0: the old copy stops at the switch", spec: withDrain(s10, "0"), revision: 1,
			copies: []CopyView{withSessions(ready(s10, 1, 0), 2), withRevision(ready(s10, 3, time.Minute), 1)},
			steps:  []string{"stop 1 pid=101 no-drain"}, active: []int{3}, switched: true},
		{name: "29 a draining copy that died is forgotten, never started again", spec: s11, revision: 1,
			copies: []CopyView{drainingSince(dead(s11, 1, exited(now.Add(-time.Second), 1)), time.Minute), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute), 1)},
			steps:  []string{"stop 1 gone"}, active: []int{3}, warm: []int{4}},
		{name: "29b ... also while its exit is pending", spec: s11, revision: 1,
			copies: []CopyView{pending(drainingSince(ready(s11, 1, 0), time.Minute)), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute), 1)},
			steps:  []string{"stop 1 gone"}, active: []int{3}, warm: []int{4}},
		{name: "30 draining copies don't count toward the copies wanted", spec: s11, revision: 1,
			copies: []CopyView{withSessions(drainingSince(ready(s11, 1, 0), time.Minute), 1), withSessions(drainingSince(ready(s11, 2, time.Second), time.Minute), 1), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute), 1)},
			active: []int{3}, warm: []int{4}, draining: []int{1, 2}},
		{name: "31 more copies with visitors draining than the app runs: the longest draining stops", spec: s10, revision: 2,
			copies: []CopyView{withSessions(drainingSince(ready(s10, 1, 0), 10*time.Minute), 2), withSessions(drainingSince(ready(s10, 2, time.Second), time.Minute), 1), withRevision(ready(s10, 3, time.Minute), 2)},
			steps:  []string{"stop 1 pid=101 too-many"}, active: []int{3}, draining: []int{2}},
		{name: "31b copies with no sessions left don't count", spec: s10, revision: 2,
			copies: []CopyView{withSessions(drainingSince(ready(s10, 1, 0), 5*time.Second), 0), withSessions(drainingSince(ready(s10, 2, time.Second), time.Minute), 1), withRevision(ready(s10, 3, time.Minute), 2)},
			active: []int{3}, draining: []int{2, 1}},
		{name: "32 a scale-down during a rollout: fewer new copies, the old spare drains", spec: s10, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(startingCopy(s11, 3, time.Minute), 1), withRevision(startingCopy(s11, 4, time.Minute), 1)},
			steps:  []string{"stop 2 pid=102 idle", "stop 4 pid=104 surplus"}, active: []int{1}, rolling: true},
		{name: "32b ... a spare that holds a visitor drains instead", spec: s10, revision: 1,
			copies: []CopyView{withSessions(ready(s11, 1, 0), 1), withSessions(ready(s11, 2, time.Second), 1), withRevision(startingCopy(s11, 3, time.Minute), 1)},
			drain:  []int{2}, active: []int{1}, draining: []int{2}, rolling: true},
		{name: "32c a restart overtakes a rollout whose new copy is ready: that copy has no visitors and stops", spec: s11, revision: 2,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Minute), withRevision(ready(s11, 3, 2*time.Minute), 1)},
			steps:  []string{"stop 3 pid=103 idle", "start 4"}, active: []int{1}, warm: []int{2}, rolling: true},
		{name: "32d R2W1: one of two new copies is ready when a restart comes", spec: s21, revision: 2,
			copies: []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second), withRevision(ready(s21, 4, time.Minute), 1)},
			steps:  []string{"stop 4 pid=104 idle", "start 5", "start 6"}, active: []int{1, 2}, warm: []int{3}, rolling: true},
		{name: "32e ... with the sessions unknown it drains", spec: s11, revision: 2,
			copies: []CopyView{unknownSessions(ready(s11, 1, 0)), unknownSessions(ready(s11, 2, time.Minute)), unknownSessions(withRevision(ready(s11, 3, 2*time.Minute), 1))},
			steps:  []string{"start 4"}, drain: []int{3}, active: []int{1}, warm: []int{2}, draining: []int{3}, rolling: true},
		{name: "33 a scale-up during a rollout: replicas new copies are wanted, two are there", spec: s21, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(startingCopy(s11, 3, time.Minute), 1), withRevision(startingCopy(s11, 4, time.Minute), 1)},
			active: []int{1, 2}, rolling: true},
		{name: "33b ... one is missing: it starts", spec: s21, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(startingCopy(s11, 3, time.Minute), 1)},
			steps:  []string{"start 4"}, active: []int{1, 2}, rolling: true},
		{name: "34 an old active copy dies before the switch: its record goes, the old spare serves", spec: s11, revision: 1,
			copies: []CopyView{withRevision(dead(s11, 1, exited(now.Add(-time.Second), 1)), 0), ready(s11, 2, time.Second), withRevision(startingCopy(s11, 3, time.Minute), 1), withRevision(startingCopy(s11, 4, time.Minute), 1)},
			steps:  []string{"stop 1 surplus"}, active: []int{2}, rolling: true},
		{name: "35 a new copy dies after the switch: an ordinary takeover", spec: s11, revision: 1,
			copies: []CopyView{withSessions(drainingSince(ready(s11, 1, 0), time.Minute), 1), withRevision(dead(s11, 3, exited(base, 1)), 1), withRevision(ready(s11, 4, time.Minute), 1)},
			steps:  []string{"start 3"}, active: []int{4}, draining: []int{1}},
		{name: "40 a rollout of a build before draining, half done: the switch at once", spec: s11, revision: 1,
			copies: []CopyView{ready(s11, 1, 0), withRevision(ready(s11, 2, time.Minute), 1)},
			steps:  []string{"start 3"}, drain: []int{1}, active: []int{2}, draining: []int{1}, switched: true},
		{name: "31c with the sessions unknown and drains that began together, the cap stops a spare, not the old active copy", spec: s11, revision: 2,
			copies: []CopyView{unknownSessions(ready(s11, 1, 0)), unknownSessions(ready(s11, 2, time.Minute)), unknownSessions(withRevision(ready(s11, 3, 2*time.Minute), 1)), unknownSessions(withRevision(ready(s11, 4, 3*time.Minute), 2))},
			steps:  []string{"stop 2 pid=102 too-many", "start 5"}, drain: []int{1, 3}, active: []int{4}, draining: []int{1, 3}, switched: true},
		{name: "41 a scale-down while copies drain: draining copies are not the surplus", spec: s10, revision: 1,
			copies: []CopyView{withSessions(drainingSince(ready(s11, 1, 0), time.Minute), 1), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute+time.Second), 1)},
			drain:  []int{4}, active: []int{3}, draining: []int{1, 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideCopies(tc.spec, Input{Revision: tc.revision, Copies: tc.copies, Now: now, SkipDrain: tc.skipDrain, Settle: DrainSettle})
			if want := tc.steps; !reflect.DeepEqual(show(got.Steps), want) && !(len(want) == 0 && len(got.Steps) == 0) {
				t.Errorf("steps = %q, want %q", show(got.Steps), want)
			}
			if !sameInts(got.Drain, tc.drain) {
				t.Errorf("drain %v, want %v", got.Drain, tc.drain)
			}
			if !sameInts(got.Active, tc.active) || !sameInts(got.Warm, tc.warm) || !sameInts(got.Draining, tc.draining) {
				t.Errorf("active %v warm %v draining %v, want %v %v %v", got.Active, got.Warm, got.Draining, tc.active, tc.warm, tc.draining)
			}
			if got.Rolling != tc.rolling || got.Switched != tc.switched {
				t.Errorf("rolling %v switched %v, want %v %v", got.Rolling, got.Switched, tc.rolling, tc.switched)
			}
		})
	}
}

func TestDescribe(t *testing.T) {
	s11, s10, s21 := copySpec(1, 1), copySpec(1, 0), copySpec(2, 1)
	step := func(typ ActionType, copy int) Step { return Step{Type: typ, Copy: copy} }
	stopStep := func(copy, pid int, why StopReason) Step {
		return Step{Type: ActionStop, Copy: copy, PID: pid, Why: why}
	}
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
		{"upgrade, a spare", s11, nil,
			Plan{Steps: []Step{{Type: ActionStop, Copy: 0, PID: 123, Why: StopUpgrade}, {Type: ActionStart, Copy: 1, Upgrade: true}, step(ActionStart, 2)}}, nil,
			`Starting app "app" on port 8080 (1 copy and 1 warm spare; moved behind the load balancer; stopped the previous instance: pid 123)`},
		{"rollout, restart", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)},
			Plan{Steps: []Step{step(ActionStart, 3), step(ActionStart, 4)}, Rolling: true, Outdated: []int{1, 2}, Waiting: []int{3, 4}, Restart: true}, nil,
			`Restarting app "app": starting copies 3 and 4; copies 1 and 2 keep serving until the new ones are ready, then drain for up to 15m`},
		{"rollout, env, one copy", s10, []CopyView{ready(s10, 1, 0)},
			Plan{Steps: []Step{step(ActionStart, 2)}, Rolling: true, Outdated: []int{1}, Waiting: []int{2}, Changed: []string{"env"}}, nil,
			`Restarting app "app" (manifest changed: env): starting copy 2; copy 1 keeps serving until the new one is ready, then drains for up to 15m`},
		{"rollout, --now", s10, []CopyView{ready(s10, 1, 0)},
			Plan{Steps: []Step{step(ActionStart, 2)}, Rolling: true, Outdated: []int{1}, Waiting: []int{2}, Restart: true, OldStop: true}, nil,
			`Restarting app "app": starting copy 2; copy 1 keeps serving until the new one is ready, then stops`},
		{"rollout, with a setting", withDrain(s10, "90s"), []CopyView{ready(s10, 1, 0)},
			Plan{Steps: []Step{step(ActionStart, 2)}, Rolling: true, Outdated: []int{1}, Changed: []string{"env"}}, []string{"replicas"},
			`Restarting app "app" (manifest changed: replicas, env): starting copy 2; copy 1 keeps serving until the new one is ready, then drains for up to 1m30s`},
		{"rollout over a rollout", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), startingCopy(s11, 3, time.Minute), startingCopy(s11, 4, time.Minute)},
			Plan{Steps: []Step{stopStep(3, 103, StopOutdated), stopStep(4, 104, StopOutdated), step(ActionStart, 5), step(ActionStart, 6)}, Rolling: true, Outdated: []int{1, 2}, Restart: true}, nil,
			`Restarting app "app": starting copies 5 and 6; copies 1 and 2 keep serving until the new ones are ready, then drain for up to 15m; stopping copies 3 and 4`},
		{"waiting", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), startingCopy(s11, 3, time.Minute), startingCopy(s11, 4, time.Minute)},
			Plan{Rolling: true, Outdated: []int{1, 2}, Waiting: []int{3, 4}}, nil,
			`Restarting app "app": waiting for copies 3 and 4 to be ready; copies 1 and 2 keep serving until then`},
		{"the switch", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), ready(s11, 3, time.Minute), ready(s11, 4, time.Minute)},
			Plan{Switched: true, Drain: []int{1, 2}, Active: []int{3}, Warm: []int{4}, Draining: []int{1, 2}}, nil,
			`Restarted app "app": copies 3 and 4 serve the new version; copies 1 and 2 drain for up to 15m`},
		{"scale down drains", s11, []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second)},
			Plan{Drain: []int{3}, Active: []int{1}, Warm: []int{2}, Draining: []int{3}}, []string{"replicas"},
			`Updated "app" without a restart (manifest changed: replicas; draining copy 3)`},
		{"scale down, a starting copy too", s11, []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second), startingCopy(s21, 4, time.Minute)},
			Plan{Steps: []Step{stopStep(4, 104, StopSurplus)}, Drain: []int{3}}, []string{"replicas"},
			`Updated "app" without a restart (manifest changed: replicas; stopping copy 4; draining copy 3)`},
		{"--now ends a drain", s10, []CopyView{drainingSince(ready(s10, 1, 0), time.Minute), ready(s10, 2, time.Minute)},
			Plan{Steps: []Step{stopStep(1, 101, StopNoDrain)}}, nil,
			`Updated "app" (stopping draining copy 1)`},
		{"a shorter drain-timeout ends a drain", s10, []CopyView{drainingSince(ready(s10, 1, 0), time.Minute), ready(s10, 2, time.Minute)},
			Plan{Steps: []Step{stopStep(1, 101, StopTimeout)}}, []string{"drain-timeout"},
			`Updated "app" without a restart (manifest changed: drain-timeout; stopping draining copy 1)`},
		{"drain-timeout alone", s10, []CopyView{ready(s10, 1, 0)}, Plan{}, []string{"drain-timeout"},
			`Updated "app" without a restart (manifest changed: drain-timeout)`},
		{"nothing", s11, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)}, Plan{}, nil,
			`Nothing to change`},
		{"a dead copy started again", s11, []CopyView{ready(s11, 1, 0), dead(s11, 2, exited(base, 1))},
			Plan{Steps: []Step{step(ActionStart, 2)}}, nil,
			`Updated "app" (starting copy 2 again)`},
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

// Every plan the decision function makes describes itself as the plan would,
// end to end: the message from DecideCopies' own plan.
func TestDescribe_FromDecideCopies(t *testing.T) {
	s := copySpec(1, 1)
	views := []CopyView{ready(s, 1, 0), ready(s, 2, time.Second)}
	p := DecideCopies(s, Input{Revision: 1, Copies: views, Now: now, Settle: DrainSettle})
	if got, want := Describe(s, views, p, nil), `Restarting app "app": starting copy 3; copies 1 and 2 keep serving until the new one is ready, then drain for up to 15m`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	p = DecideCopies(s, Input{Revision: 1, Copies: views, Now: now, Settle: DrainSettle, SkipDrain: true})
	if got, want := Describe(s, views, p, nil), `Restarting app "app": starting copy 3; copies 1 and 2 keep serving until the new one is ready, then stop`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// Two copies and a spare: two new copies first.
	s21 := copySpec(2, 1)
	views = []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second)}
	p = DecideCopies(s21, Input{Revision: 1, Copies: views, Now: now, Settle: DrainSettle})
	if got, want := Describe(s21, views, p, nil), `Restarting app "app": starting copies 4 and 5; copies 1, 2 and 3 keep serving until the new ones are ready, then drain for up to 15m`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// The switch: the old active copy drains, the old spare has no visitors
	// and stops, the new spare starts.
	views = []CopyView{ready(s, 1, 0), ready(s, 2, time.Second), withRevision(ready(s, 3, time.Minute), 1)}
	p = DecideCopies(s, Input{Revision: 1, Copies: views, Now: now, Settle: DrainSettle})
	if got, want := Describe(s, views, p, nil), `Restarted app "app": copy 3 serves the new version; copy 1 drains for up to 15m; starting copy 4; stopping copy 2`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		15 * time.Minute: "15m", 90 * time.Second: "1m30s", time.Hour: "1h", 90 * time.Minute: "1h30m", 0: "0s", 24 * time.Hour: "24h", 5 * time.Second: "5s",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestDescribeRoles(t *testing.T) {
	for _, tc := range []struct {
		active, warm, draining []int
		want                   string
	}{
		{nil, nil, nil, "no copy serves visitors yet"},
		{[]int{1}, nil, nil, "copy 1 serves visitors"},
		{[]int{2, 3}, []int{1}, nil, "copies 2 and 3 serve visitors; copy 1 is warm"},
		{[]int{3}, []int{4}, []int{1, 2}, "copy 3 serves visitors; copy 4 is warm; copies 1 and 2 are draining"},
		{nil, nil, []int{1}, "no copy serves visitors yet; copy 1 is draining"},
	} {
		if got := DescribeRoles(tc.active, tc.warm, tc.draining); got != tc.want {
			t.Errorf("DescribeRoles(%v, %v, %v) = %q, want %q", tc.active, tc.warm, tc.draining, got, tc.want)
		}
	}
}

// Roles is DecideCopies' roles alone, for rebuilding the load balancer's table.
func TestRoles(t *testing.T) {
	s11, s10, s21 := copySpec(1, 1), copySpec(1, 0), copySpec(2, 1)
	cases := []struct {
		name                   string
		spec                   manifest.AppSpec
		revision               int
		copies                 []CopyView
		active, warm, draining []int
	}{
		{"an app at rest", s11, 0, []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)}, []int{1}, []int{2}, nil},
		{"the switch: the new copy is active, the old one drains", s11, 1,
			[]CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(ready(s11, 3, time.Minute), 1)}, []int{3}, nil, []int{1}},
		{"copies draining after the switch", s11, 1,
			[]CopyView{withSessions(drainingSince(ready(s11, 1, 0), time.Minute), 1), withSessions(drainingSince(ready(s11, 2, time.Second), time.Minute), 1), withRevision(ready(s11, 3, time.Minute), 1), withRevision(ready(s11, 4, time.Minute), 1)},
			[]int{3}, []int{4}, []int{1, 2}},
		{"a copy that is starting has no role", s10, 0, []CopyView{ready(s10, 1, 0), startingCopy(s10, 2, time.Second)}, []int{1}, nil, nil},
		{"two copies and a spare", s21, 0, []CopyView{ready(s21, 1, 0), ready(s21, 2, time.Second), ready(s21, 3, 2*time.Second)}, []int{1, 2}, []int{3}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, w, d := Roles(tc.spec, Input{Revision: tc.revision, Copies: tc.copies, Now: now, Settle: DrainSettle})
			if !sameInts(a, tc.active) || !sameInts(w, tc.warm) || !sameInts(d, tc.draining) {
				t.Errorf("roles = %v %v %v, want %v %v %v", a, w, d, tc.active, tc.warm, tc.draining)
			}
		})
	}
}

// What the caller's reply and log make of a plan.
func TestPlan_Action(t *testing.T) {
	step := func(typ ActionType) Step { return Step{Type: typ, Copy: 1} }
	cases := []struct {
		name string
		plan Plan
		want ActionType
	}{
		{"nothing", Plan{}, ActionNone},
		{"a start", Plan{Steps: []Step{step(ActionStart)}}, ActionStart},
		{"a start and a stop", Plan{Steps: []Step{step(ActionStop), step(ActionStart)}}, ActionStart},
		{"only a stop", Plan{Steps: []Step{step(ActionStop)}}, ActionNone},
		{"only drains", Plan{Drain: []int{1}}, ActionNone},
		{"a backoff", Plan{Steps: []Step{step(ActionBackoff)}}, ActionBackoff},
		{"a backoff with a stop", Plan{Steps: []Step{step(ActionBackoff), step(ActionStop)}}, ActionNone},
		{"a backoff with a drain", Plan{Steps: []Step{step(ActionBackoff)}, Drain: []int{2}}, ActionNone},
	}
	for _, tc := range cases {
		if got := tc.plan.Action(); got != tc.want {
			t.Errorf("%s: Action() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A rollout names the copies it waits for and the old ones that serve.
func TestDecideCopies_WaitingAndOutdated(t *testing.T) {
	s11 := copySpec(1, 1)
	copies := []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second), withRevision(startingCopy(s11, 3, time.Minute), 1), withRevision(startingCopy(s11, 4, time.Minute), 1)}
	p := DecideCopies(s11, Input{Revision: 1, Copies: copies, Now: now, Settle: DrainSettle})
	if !p.Rolling || !sameInts(p.Waiting, []int{3, 4}) || !sameInts(p.Outdated, []int{1, 2}) || !p.Restart {
		t.Errorf("plan = %+v, want rolling, waiting for 3 and 4, with 1 and 2 outdated by a restart", p)
	}
	// One new copy, as a restart starts it: it is the one waited for.
	p = DecideCopies(s11, Input{Revision: 1, Copies: []CopyView{ready(s11, 1, 0), ready(s11, 2, time.Second)}, Now: now, Settle: DrainSettle})
	if !sameInts(p.Waiting, []int{3}) || !sameInts(p.Outdated, []int{1, 2}) {
		t.Errorf("plan = %+v, want it waiting for copy 3", p)
	}
}
