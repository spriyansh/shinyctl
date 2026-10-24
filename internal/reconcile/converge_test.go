package reconcile

import (
	"math/rand"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

// A simulated daemon: each round makes a plan from the copies, carries it out
// (a stop removes a copy, a start or a replacement gives a starting copy of
// the current revision), then starting copies become ready, or, when the new
// version is broken, die at once and are started again after their backoff.
// It checks what the plans do over many random beginnings (any mix of old and
// new copies, ready or not, and any scale-down or scale-up):
//   - a healthy new version always ends with exactly the wanted number of
//     copies, all ready and on the new version (so the copies kept over the
//     number do go away), and never leaves no ready copy while one existed and
//     a copy of the new version was alive;
//   - a broken new version never takes the last ready copy away: the old one
//     keeps serving and the rollout stalls.
type simCopy struct {
	n, rev    int
	alive     bool
	ready     bool
	started   time.Time
	crashes   int
	lastExit  time.Time
	hasExited bool
}

func simViews(s manifest.AppSpec, cs []simCopy) []CopyView {
	var vs []CopyView
	for _, c := range cs {
		a := rec(s, c.n, 0, c.rev)
		a.StartedAt = c.started
		a.CrashCount = c.crashes
		if c.hasExited {
			a.LastExitAt, a.LastExitFailed = c.lastExit, true
		}
		vs = append(vs, CopyView{App: a, Alive: c.alive, Ready: c.ready})
	}
	return vs
}

func simRun(t *testing.T, rng *rand.Rand, run int, broken bool) {
	const revision = 1
	r, w := 1+rng.Intn(3), rng.Intn(3)
	s := copySpec(r, w)
	total := s.CopyCount()
	clock := base
	var cs []simCopy
	for n := 1; n <= 1+rng.Intn(6); n++ {
		c := simCopy{n: n, rev: rng.Intn(2), alive: true, ready: rng.Intn(3) > 0, started: base.Add(time.Duration(rng.Intn(60)) * time.Second)}
		if broken && c.rev == revision {
			// A copy of a broken new version is never ready.
			c.ready = false
		}
		cs = append(cs, c)
	}

	for round := 0; round < 40; round++ {
		clock = clock.Add(time.Second)
		views := simViews(s, cs)
		oldReady, hadReady, curAlive, curExists := false, false, false, false
		for _, c := range cs {
			hadReady = hadReady || (c.alive && c.ready)
			oldReady = oldReady || (c.alive && c.ready && c.rev != revision)
			curAlive = curAlive || (c.alive && c.rev == revision)
			curExists = curExists || c.rev == revision
		}
		p := DecideCopies(s, revision, views, clock)

		byN := map[int]int{}
		for i, c := range cs {
			byN[c.n] = i
		}
		gone := map[int]bool{}
		for _, st := range p.Steps {
			switch st.Type {
			case ActionStop:
				gone[st.Copy] = true
			case ActionStart:
				fresh := simCopy{n: st.Copy, rev: revision, alive: true, started: clock}
				if i, ok := byN[st.Copy]; ok {
					fresh.crashes, fresh.lastExit, fresh.hasExited = cs[i].crashes, cs[i].lastExit, cs[i].hasExited
					cs[i] = fresh
				} else {
					cs = append(cs, fresh)
					byN[st.Copy] = len(cs) - 1
				}
			}
		}
		var next []simCopy
		for _, c := range cs {
			if !gone[c.n] {
				next = append(next, c)
			}
		}
		cs = next

		stillReady := false
		for _, c := range cs {
			stillReady = stillReady || (c.alive && c.ready)
		}
		if broken {
			// (A lone old copy with nothing of the new version yet is replaced, as
			// it always was: that is a restart, not a stall.)
			if oldReady && curExists && !stillReady {
				t.Fatalf("run %d round %d: spec %d+%d: the plan %v took the last ready copy away for a version that is broken", run, round, r, w, show(p.Steps))
			}
		} else if hadReady && curAlive && !stillReady {
			t.Fatalf("run %d round %d: spec %d+%d: a ready copy existed and a copy of the new version was alive, and the plan %v left none ready", run, round, r, w, show(p.Steps))
		}

		if !broken && len(p.Steps) == 0 && len(cs) == total {
			settled := true
			for _, c := range cs {
				settled = settled && c.alive && c.ready && c.rev == revision
			}
			if settled {
				return
			}
		}

		for i := range cs {
			c := &cs[i]
			if !c.alive || c.ready || c.rev != revision {
				continue
			}
			switch {
			case broken:
				c.alive, c.crashes, c.lastExit, c.hasExited = false, c.crashes+1, clock, true
			case rng.Intn(2) == 0:
				c.ready = true
			}
		}
	}
	if !broken {
		t.Fatalf("run %d: spec %d+%d did not settle: %+v", run, r, w, cs)
	}
}

// 10,000 runs of each kind take about a second.
func TestDecideCopies_SimulationConverges(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for run := 0; run < 10000; run++ {
		simRun(t, rng, run, false)
	}
}

func TestDecideCopies_SimulationBrokenVersionStalls(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	for run := 0; run < 10000; run++ {
		simRun(t, rng, run, true)
	}
}

// A second simulated daemon with a broken new version, over every scale: a
// copy started at the new revision crashes one round later, every time, and
// is started again after its backoff (restart-policy always) or left alone
// (never). Once a copy of the new version exists, a ready copy that exists
// before a round must exist after it, and the number of copies stays bounded
// (at most twice the number wanted: the deferral's limit).
type bcopy struct {
	n, rev       int
	alive, ready bool
	started      time.Time
	lastExit     time.Time
	crashes      int
}

func TestDecideCopies_SimulationBrokenVersionKeepsServing(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	for run := 0; run < 10000; run++ {
		r, w := 1+rng.Intn(3), rng.Intn(3)
		spec := copySpec(r, w)
		if rng.Intn(2) == 0 {
			spec.RestartPolicy = manifest.RestartNever
		}
		T := spec.CopyCount()
		clock := base.Add(time.Hour)
		var cs []bcopy
		for n := 1; n <= 1+rng.Intn(6); n++ {
			c := bcopy{n: n, rev: rng.Intn(2), started: base.Add(time.Duration(rng.Intn(60)) * time.Second)}
			if c.rev == 0 {
				c.alive, c.ready = true, rng.Intn(4) > 0
			} else {
				c.alive = rng.Intn(2) == 0 // a new copy: starting, or dead
				if !c.alive {
					c.lastExit, c.crashes = clock.Add(-time.Second), 1
				}
			}
			cs = append(cs, c)
		}
		const revision = 1
		for round := 0; round < 30; round++ {
			clock = clock.Add(3 * time.Second)
			var views []CopyView
			hadReady, hadNew := false, false
			for _, c := range cs {
				hadNew = hadNew || c.rev == 1
				a := rec(spec, c.n, 0, c.rev)
				a.StartedAt, a.LastExitAt, a.CrashCount = c.started, c.lastExit, c.crashes
				if !c.lastExit.IsZero() {
					a.LastExitFailed = true
				}
				if !c.alive {
					a.PID = 0
				}
				views = append(views, CopyView{App: a, Alive: c.alive, Ready: c.ready})
				hadReady = hadReady || c.ready
			}
			p := DecideCopies(spec, revision, views, clock)
			gone := map[int]bool{}
			idx := map[int]int{}
			for i, c := range cs {
				idx[c.n] = i
			}
			for _, st := range p.Steps {
				switch st.Type {
				case ActionStop:
					gone[st.Copy] = true
				case ActionStart:
					c := bcopy{n: st.Copy, rev: revision, alive: true, started: clock}
					if i, ok := idx[st.Copy]; ok {
						c.lastExit, c.crashes = cs[i].lastExit, cs[i].crashes
						cs[i] = c
					} else {
						cs = append(cs, c)
						idx[st.Copy] = len(cs) - 1
					}
				}
			}
			var next []bcopy
			for _, c := range cs {
				if !gone[c.n] {
					next = append(next, c)
				}
			}
			cs = next
			stillReady := false
			for _, c := range cs {
				stillReady = stillReady || c.ready
			}
			// With no copy of the new version yet, replacing a lone old copy
			// is a plain restart (one copy wanted), not a stall.
			if hadReady && hadNew && !stillReady {
				t.Fatalf("run %d round %d: spec %d+%d %s: the plan %v took the last ready copy", run, round, r, w, spec.RestartPolicy, show(p.Steps))
			}
			if len(cs) > 2*T+1 {
				t.Fatalf("run %d round %d: %d copies for %d wanted", run, round, len(cs), T)
			}
			// the broken new version: a current copy that was started crashes
			for i := range cs {
				if cs[i].rev == revision && cs[i].alive && cs[i].started.Before(clock) {
					cs[i].alive, cs[i].ready = false, false
					cs[i].lastExit = clock.Add(time.Second)
					cs[i].crashes++
				}
			}
		}
	}
}
