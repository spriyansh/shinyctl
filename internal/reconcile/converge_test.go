package reconcile

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

// A simulated daemon. Each round makes a plan from the copies and carries it
// out: a stop removes a copy, a drain marks it, a start gives a starting copy
// of the current revision (in place, with its crash history, or new). Then
// the world moves: starting copies become ready (or, for a broken version,
// die), visitors open and close sessions, and now and then something happens:
// a restart, a scale change, a --now, a crash, the load balancer not
// answering.
type sim struct {
	t        *testing.T
	rng      *rand.Rand
	spec     manifest.AppSpec
	revision int
	clock    time.Time
	copies   []*simCopy
	broken   bool // copies of the current revision never get ready
	unknown  bool // the load balancer doesn't answer this round
}

type simCopy struct {
	n, rev      int
	alive       bool
	ready       bool
	started     time.Time
	crashes     int
	lastExit    time.Time
	since       time.Time // draining since
	skip        bool
	sessions    int
	heldAtDrain bool // it had visitors when it began draining
}

func (s *sim) views() []CopyView {
	var vs []CopyView
	for _, c := range s.copies {
		a := rec(s.spec, c.n, 0, c.rev)
		a.StartedAt, a.CrashCount, a.LastExitAt = c.started, c.crashes, c.lastExit
		a.LastExitFailed = !c.lastExit.IsZero()
		a.DrainingSince, a.SkipDrain = c.since, c.skip
		v := CopyView{App: a, Alive: c.alive, Ready: c.alive && c.ready}
		if !s.unknown {
			v.SessionsKnown = true
			if c.alive {
				v.Sessions = c.sessions
			}
		}
		vs = append(vs, v)
	}
	return vs
}

func (s *sim) byN(n int) *simCopy {
	for _, c := range s.copies {
		if c.n == n {
			return c
		}
	}
	return nil
}

func (s *sim) current(c *simCopy) bool { return c.rev == s.revision && c.since.IsZero() }

// round makes and carries out one plan, checking it, and returns it.
func (s *sim) round(skipDrain bool) Plan {
	t := s.t
	views := s.views()
	p := DecideCopies(s.spec, Input{Revision: s.revision, Copies: views, Now: s.clock, SkipDrain: skipDrain, Settle: DrainSettle})
	R, T := s.spec.EffectiveReplicas(), s.spec.CopyCount()
	timeout := s.spec.EffectiveDrainTimeout()

	inInput := map[int]bool{}
	readyBefore := false
	for _, c := range s.copies {
		inInput[c.n] = true
		readyBefore = readyBefore || (c.alive && c.ready && c.since.IsZero())
	}

	stopped := map[int]bool{}
	for _, st := range p.Steps {
		c := s.byN(st.Copy)
		switch st.Type {
		case ActionStop:
			stopped[st.Copy] = true
			if c != nil && !c.since.IsZero() && c.alive && !s.unknown && c.sessions > 0 {
				ok := st.Why == StopTimeout && s.clock.Sub(c.since) >= timeout ||
					st.Why == StopNoDrain && (skipDrain || c.skip || timeout == 0) ||
					st.Why == StopTooMany
				if !ok {
					t.Fatalf("a draining copy with %d sessions was stopped (%s) after %v: %v", c.sessions, st.Why, s.clock.Sub(c.since), show(p.Steps))
				}
			}
			if c != nil && c.since.IsZero() && c.alive && c.ready && !(skipDrain || c.skip || timeout == 0) && st.Why != StopTooMany && st.Why != StopIdle {
				t.Fatalf("a ready copy was stopped (%s) instead of drained: %v", st.Why, show(p.Steps))
			}
		case ActionStart:
			if c != nil && !c.since.IsZero() {
				t.Fatalf("draining copy %d started again: %v", st.Copy, show(p.Steps))
			}
			if c == nil && inInput[st.Copy] {
				t.Fatalf("copy number %d reused in one plan: %v", st.Copy, show(p.Steps))
			}
		}
	}
	for _, n := range p.Drain {
		if c := s.byN(n); c == nil || !c.since.IsZero() || !c.alive || !c.ready {
			t.Fatalf("drain of copy %d, which is not a ready copy outside a drain", n)
		}
	}

	// Carry it out.
	s.copies = slices.DeleteFunc(s.copies, func(c *simCopy) bool { return stopped[c.n] })
	for _, n := range p.Drain {
		c := s.byN(n)
		c.since, c.heldAtDrain = s.clock, c.sessions > 0
	}
	for _, st := range p.Steps {
		if st.Type != ActionStart {
			continue
		}
		if c := s.byN(st.Copy); c != nil {
			*c = simCopy{n: c.n, rev: s.revision, alive: true, started: s.clock, crashes: c.crashes, lastExit: c.lastExit}
		} else {
			s.copies = append(s.copies, &simCopy{n: st.Copy, rev: s.revision, alive: true, started: s.clock})
		}
	}

	// What visitors see now.
	listed := map[int]bool{}
	for _, n := range slices.Concat(p.Active, p.Warm, p.Draining) {
		if listed[n] {
			t.Fatalf("copy %d listed twice: active %v warm %v draining %v", n, p.Active, p.Warm, p.Draining)
		}
		listed[n] = true
		c := s.byN(n)
		if c == nil || !c.alive {
			t.Fatalf("copy %d is listed but not running", n)
		}
	}
	for _, n := range p.Draining {
		if s.byN(n).since.IsZero() {
			t.Fatalf("copy %d listed as draining without draining", n)
		}
	}
	readyAfter, currentReady := false, 0
	for _, c := range s.copies {
		if c.alive && c.ready && c.since.IsZero() {
			readyAfter = true
		}
		if c.alive && c.ready && s.current(c) {
			currentReady++
		}
	}
	if readyBefore && !readyAfter {
		t.Fatalf("the plan %v left no ready copy outside a drain", show(p.Steps))
	}
	if readyAfter && len(p.Active) == 0 {
		t.Fatalf("a copy is ready but none is active: %v", show(p.Steps))
	}
	if currentReady >= R {
		for _, n := range p.Active {
			if !s.current(s.byN(n)) {
				t.Fatalf("%d copies of the new version are ready, yet copy %d of a previous version takes new visitors", currentReady, n)
			}
		}
	}
	busy := 0
	for _, c := range s.copies {
		if !c.since.IsZero() && c.alive && (s.unknown || c.sessions > 0) {
			busy++
		}
	}
	if busy > T {
		t.Fatalf("%d copies with visitors drain, for %d wanted", busy, T)
	}
	// Copies that drain with no sessions left go within the settle time;
	// the rest are bounded: the copies wanted, old ones still serving (at most
	// as many), and draining ones with visitors (at most as many).
	idle := 0
	for _, c := range s.copies {
		if !c.since.IsZero() && !s.unknown && c.sessions == 0 {
			idle++
		}
	}
	if len(s.copies)-idle > 3*T {
		t.Fatalf("%d copies for %d wanted (rev %d, plan %v drain %v): %s", len(s.copies), T, s.revision, show(p.Steps), p.Drain, s.dump())
	}
	return p
}

// world moves on by one round.
func (s *sim) world() {
	s.clock = s.clock.Add(time.Duration(1+s.rng.Intn(3)) * time.Second)
	for _, c := range s.copies {
		switch {
		case !c.alive:
		case !c.ready && c.rev == s.revision && s.broken:
			c.alive, c.crashes, c.lastExit = false, c.crashes+1, s.clock
		case !c.ready && s.rng.Intn(2) == 0:
			c.ready = true
		case c.ready && !c.since.IsZero() && c.sessions > 0 && s.rng.Intn(3) == 0:
			c.sessions--
		case c.ready && c.since.IsZero() && s.rng.Intn(4) == 0:
			c.sessions++
		}
	}
}

func newSim(t *testing.T, rng *rand.Rand) *sim {
	s := newSimFor(t, rng, 1+rng.Intn(3), rng.Intn(3))
	if rng.Intn(3) == 0 {
		s.spec.DrainTimeout = []string{"0", "30s", "2m"}[rng.Intn(3)]
	}
	return s
}

// newSimFor is a sim of an app with replicas r and warm-replicas w, running
// its copies of revision 1.
func newSimFor(t *testing.T, rng *rand.Rand, r, w int) *sim {
	s := &sim{t: t, rng: rng, clock: base.Add(time.Hour)}
	s.spec = copySpec(r, w)
	s.revision = 1
	for n := 1; n <= s.spec.CopyCount(); n++ {
		s.copies = append(s.copies, &simCopy{n: n, rev: 1, alive: true, ready: true, started: base.Add(time.Duration(n) * time.Second), sessions: rng.Intn(3)})
	}
	return s
}

// A healthy new version: whatever happens on the way, once nothing more
// happens the app settles at the copies wanted, all of the new version, with
// nothing draining.
func TestDecideCopies_SimulationConverges(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for run := 0; run < 5000; run++ {
		s := newSim(t, rng)
		events := 3 + rng.Intn(6)
		for round := 0; round < 400; round++ {
			skipDrain := false
			if events > 0 && rng.Intn(4) == 0 {
				events--
				switch rng.Intn(6) {
				case 0, 1:
					s.revision++ // restart
				case 2:
					s.spec = copySpec(1+rng.Intn(3), rng.Intn(3))
				case 3:
					s.revision++
					skipDrain = true // restart --now
					for _, c := range s.copies {
						if c.alive && (c.rev < s.revision || !c.since.IsZero()) {
							c.skip = true
						}
					}
				case 4:
					if len(s.copies) > 0 {
						c := s.copies[rng.Intn(len(s.copies))]
						c.alive, c.ready, c.sessions, c.lastExit, c.crashes = false, false, 0, s.clock, c.crashes+1
					}
				case 5:
					s.unknown = true
				}
			}
			s.round(skipDrain)
			s.unknown = false
			s.world()
			if events == 0 && s.settled() {
				break
			}
		}
		if !s.settled() {
			t.Fatalf("run %d: spec %d+%d drain %q did not settle: %s", run, s.spec.EffectiveReplicas(), s.spec.EffectiveWarm(), s.spec.DrainTimeout, s.dump())
		}
	}
}

func (s *sim) anyDraining() bool {
	for _, c := range s.copies {
		if !c.since.IsZero() {
			return true
		}
	}
	return false
}

func (s *sim) settled() bool {
	if len(s.copies) != s.spec.CopyCount() {
		return false
	}
	for _, c := range s.copies {
		if !c.alive || !c.ready || !s.current(c) {
			return false
		}
	}
	return true
}

func (s *sim) dump() string {
	var out string
	for _, c := range s.copies {
		out += "\n  " + simString(c)
	}
	return out
}

func simString(c *simCopy) string {
	return fmt.Sprintf("copy %d rev %d alive %v ready %v draining %v skip %v sessions %d started %s", c.n, c.rev, c.alive, c.ready, !c.since.IsZero(), c.skip, c.sessions, c.started.Format("15:04:05"))
}

// A broken new version: the old copies keep serving, whatever else happens
// (scale changes, --now), and the copies stay bounded.
func TestDecideCopies_SimulationBrokenVersionKeepsServing(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	for run := 0; run < 5000; run++ {
		s := newSim(t, rng)
		if rng.Intn(2) == 0 {
			s.spec.RestartPolicy = manifest.RestartNever
		}
		s.revision++ // the broken version
		s.broken = true
		for round := 0; round < 60; round++ {
			skipDrain := false
			switch rng.Intn(12) {
			case 0:
				spec := copySpec(1+rng.Intn(3), rng.Intn(3))
				spec.RestartPolicy, spec.DrainTimeout = s.spec.RestartPolicy, s.spec.DrainTimeout
				s.spec = spec
			case 1:
				skipDrain = true
			}
			s.round(skipDrain)
			s.world()
			oldServing := 0
			for _, c := range s.copies {
				if c.alive && c.ready && c.since.IsZero() {
					oldServing++
				}
			}
			if oldServing == 0 {
				t.Fatalf("run %d round %d: no copy of the previous version serves any more: %s", run, round, s.dump())
			}
		}
	}
}

// An old copy that is stopped at the switch has no visitors: no sessions, and
// it is a spare (it takes none unless it stands in, and then it has sessions).
func TestDecideCopies_SimulationIdleStopsHaveNoVisitors(t *testing.T) {
	rng := rand.New(rand.NewSource(17))
	for run := 0; run < 2000; run++ {
		s := newSim(t, rng)
		s.revision++
		for round := 0; round < 40; round++ {
			views := s.views()
			p := DecideCopies(s.spec, Input{Revision: s.revision, Copies: views, Now: s.clock, Settle: DrainSettle})
			for _, st := range p.Steps {
				if st.Why != StopIdle {
					continue
				}
				c := s.byN(st.Copy)
				if c == nil || c.sessions != 0 || !c.since.IsZero() || slices.Contains(p.Active, st.Copy) {
					t.Fatalf("run %d: idle stop of copy %d: %s", run, st.Copy, s.dump())
				}
			}
			s.round(false)
			s.world()
		}
	}
}

// The peak of a restart. Once nothing else happens, a restart runs at most
// replicas + warm-replicas + replicas processes at once (3 with the defaults),
// not counting old copies that still hold visitors: the new version starts
// with the serving copies only, its spares start after the switch, and an old
// spare with no sessions stops at the switch.
func TestDecideCopies_SimulationRestartPeak(t *testing.T) {
	rng := rand.New(rand.NewSource(19))
	peakSeen := 0
	for run := 0; run < 3000; run++ {
		s := newSim(t, rng)
		restartPeak(t, rng, s, run, &peakSeen, 0)
	}
	t.Logf("the most processes at once without visitors: %d", peakSeen)
}

// With the defaults, one copy and one spare, a restart never runs more than
// three R processes at once, however long old pages stay open.
func TestDecideCopies_SimulationRestartPeakWithTheDefaults(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	peakSeen := 0
	for run := 0; run < 3000; run++ {
		s := newSimFor(t, rng, 1, 1)
		if rng.Intn(3) == 0 {
			s.spec.DrainTimeout = []string{"0", "30s", "2m"}[rng.Intn(3)]
		}
		restartPeak(t, rng, s, run, &peakSeen, 0)
	}
	if peakSeen != 3 {
		t.Errorf("the most processes at once without visitors: %d, want 3 (and never more)", peakSeen)
	}
}

func restartPeak(t *testing.T, rng *rand.Rand, s *sim, run int, peakSeen *int, overtakeAt int) {
	t.Helper()
	overtaken := overtakeAt == 0
	R, T := s.spec.EffectiveReplicas(), s.spec.CopyCount()
	s.revision++ // one restart, then nothing else
	skip := rng.Intn(4) == 0
	if skip {
		for _, c := range s.copies {
			c.skip = true
		}
	}
	for round := 0; round < 400; round++ {
		if overtakeAt > 0 && round >= overtakeAt && !overtaken && !s.anyDraining() {
			s.revision++ // a second restart before the first has switched
			overtaken = true
		}
		s.round(skip)
		n := 0
		for _, c := range s.copies {
			old := c.rev < s.revision || !c.since.IsZero()
			if c.alive && !(old && (c.sessions > 0 || c.heldAtDrain)) {
				n++
			}
		}
		*peakSeen = max(*peakSeen, n)
		if n > T+R {
			t.Fatalf("run %d (%d+%d): %d processes without visitors at once, want at most %d: %s", run, R, T-R, n, T+R, s.dump())
		}
		s.world()
		if s.settled() && overtaken {
			break
		}
	}
	if !s.settled() {
		t.Fatalf("run %d did not settle: %s", run, s.dump())
	}
}

// A restart that overtakes a rollout that has not switched yet, in any round,
// keeps the bound too: a new
// copy that is ready but has not taken over yet is an idle spare of the newest
// restart and stops at once.
func TestDecideCopies_SimulationOvertakingRestartPeak(t *testing.T) {
	rng := rand.New(rand.NewSource(29))
	peakSeen := 0
	for run := 0; run < 3000; run++ {
		s := newSimFor(t, rng, 1+rng.Intn(3), rng.Intn(3))
		restartPeak(t, rng, s, run, &peakSeen, 1+rng.Intn(8))
	}
	t.Logf("the most processes at once without visitors: %d", peakSeen)
}

// With the defaults, a second restart a round after the first (the new copy
// ready, not switched yet) runs three processes at most.
func TestDecideCopies_SimulationOvertakingRestartPeakWithTheDefaults(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	peakSeen := 0
	for run := 0; run < 3000; run++ {
		s := newSimFor(t, rng, 1, 1)
		restartPeak(t, rng, s, run, &peakSeen, 1+rng.Intn(8))
	}
	if peakSeen > 3 {
		t.Errorf("the most processes at once without visitors: %d, want 3 at most", peakSeen)
	}
}
