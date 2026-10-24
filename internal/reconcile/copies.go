package reconcile

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

// CopyView is what the daemon observed about one recorded copy.
type CopyView struct {
	App      state.AppState
	Alive    bool // running, and identifiably this copy (entrypoint and port)
	Ready    bool // alive and accepting connections on its internal port
	Pending  bool // not alive, and its exit-watcher has yet to record the exit
	Sessions int  // open connections to it through the load balancer
}

// Step is one thing DecideCopies wants done.
type Step struct {
	Type    ActionType // ActionStart, ActionStop or ActionBackoff
	Copy    int        // the copy number; 0 only for a Stop of a record from before the load balancer
	PID     int        // the live process this step stops (Stop, or a Start with Replace); 0 if none
	Replace bool       // Start: stop the copy's live process first
	Changed []string   // Start with Replace: the manifest fields behind it
	Restart bool       // Start with Replace: a restart asked for it (the revision)
	Upgrade bool       // Start of copy 1 replacing a process from before the load balancer
}

// Plan is DecideCopies' answer.
type Plan struct {
	Steps   []Step // Stops (fewer copies, or the old process of an upgrade), then the replacement, then Starts and Backoffs, each in copy order
	Active  []int  // copies new visitors go to, in role order
	Warm    []int  // ready spares, in role order
	Rolling bool   // an outdated copy is waiting for the current copies to be ready

	// Waiting names the copies Rolling waits for, and Outdated the copies
	// that run a previous version, for the reply.
	Waiting  []int
	Outdated []int
}

// Action is what the plan comes to for the caller's reply: a start if any
// copy is started, a backoff if that is all that is going on, else nothing.
func (p Plan) Action() ActionType {
	backoff := false
	for _, s := range p.Steps {
		switch s.Type {
		case ActionStart:
			return ActionStart
		case ActionBackoff:
			backoff = true
		}
	}
	if backoff && !slices.ContainsFunc(p.Steps, func(s Step) bool { return s.Type == ActionStop }) {
		return ActionBackoff
	}
	return ActionNone
}

// item is a copy with what DecideCopies works out about it.
type item struct {
	CopyView
	outdated bool
	changed  []string
	restart  bool
	gaveUp   bool
	class    int  // 0 no process, 1 starting, 2 ready; see stopCompare
	active   bool // ready, and among the first R by started-at
}

// DecideCopies is the reconcile loop's pure decision function: given what
// spec wants, the state's revision and what was observed of each recorded
// copy, it says what to do, never does it itself. now is the caller's
// time.Now(), passed in so this stays deterministic.
//
// The rules, in order:
//  1. A live record without a copy number is a process from before the load
//     balancer, holding the app's port itself. It is stopped and the app
//     starts again behind the load balancer, with every copy.
//  2. Each copy is outdated (alive, but started from an older revision or
//     another manifest) or given up (dead, and the restart policy brings
//     nothing back).
//  3. Roles are worked out for every ready copy, to rank the copies for
//     removal.
//  4. With more copies than wanted, the surplus is stopped, in this order:
//     copies that run the previous version, while no copy of the new version
//     is ready yet (so a crashed new copy is the one kept and retried); copies
//     with no process; copies that run the previous version; starting copies;
//     ready copies with the fewest sessions (a warm one before an active one
//     when they tie). If that would leave none of the kept copies ready, up
//     to one old ready copy for each kept copy stays over the number, the
//     active one before the warm one, until a copy of the new version is
//     ready.
//  5. With fewer, the lowest free copy numbers are started.
//  6. A dead copy is started again in place, after its backoff.
//  7. Of the copies that are alive, ready and not being replaced, the oldest
//     R are active and the rest warm. A restarted copy is the newest, so it
//     comes back as a spare and roles never swap back on their own.
//  8. Outdated copies are replaced one at a time, and only when every
//     current copy is alive and ready, so a broken new version stalls with
//     the old copies still serving.
func DecideCopies(spec manifest.AppSpec, revision int, copies []CopyView, now time.Time) Plan {
	total := spec.CopyCount()

	// Rule 1.
	for _, c := range copies {
		if c.App.Copy == 0 && c.Alive {
			steps := []Step{
				{Type: ActionStop, Copy: 0, PID: c.App.PID},
				{Type: ActionStart, Copy: 1, Upgrade: true},
			}
			for n := 2; n <= total; n++ {
				steps = append(steps, Step{Type: ActionStart, Copy: n})
			}
			return Plan{Steps: steps}
		}
	}

	// A dead record from before the load balancer is copy 1 again, with its
	// crash history; its port was the app's own, so it has none to keep.
	views := slices.Clone(copies)
	for i := range views {
		if views[i].App.Copy == 0 {
			views[i].App.Copy, views[i].App.Port = 1, 0
		}
	}
	slices.SortStableFunc(views, func(a, b CopyView) int { return a.App.Copy - b.App.Copy })

	// Rules 2 and 3.
	items := make([]item, len(views))
	for i, v := range views {
		it := item{CopyView: v}
		if v.Alive {
			it.changed, it.restart = Outdated(spec, revision, v.App)
			it.outdated = len(it.changed) > 0 || it.restart
		} else if !v.Pending {
			it.gaveUp = GaveUp(spec, v.App)
		}
		items[i] = it
	}
	active, _ := rankRoles(views, spec.EffectiveReplicas())
	for i := range items {
		switch {
		case !items[i].Alive:
			items[i].class = 0
		case !items[i].Ready:
			items[i].class = 1
		default:
			items[i].class = 2
			items[i].active = slices.Contains(active, items[i].App.Copy)
		}
	}

	var plan Plan

	// Rule 4.
	kept := items
	if len(items) > total {
		order := slices.Clone(items)
		newReady := slices.ContainsFunc(items, func(it item) bool { return it.Alive && it.Ready && !it.outdated })
		slices.SortStableFunc(order, surplusCompare(newReady))
		removed := map[int]bool{}
		for _, it := range order[:len(items)-total] {
			removed[it.App.Copy] = true
		}

		// Old-version copies go first, which can leave no ready copy among the
		// ones kept (the new ones are still starting, or crashed). The old
		// ready ones then stay for now, over the number wanted: at most one
		// for each wanted copy that is not ready, the active one before the
		// warm one. They go once the new ones are ready; if the new version
		// never gets ready, they keep serving and the rollout stays stalled.
		if !slices.ContainsFunc(items, func(it item) bool { return !removed[it.App.Copy] && it.Alive && it.Ready }) {
			var spare []item
			for _, it := range items {
				if removed[it.App.Copy] && it.Alive && it.Ready {
					spare = append(spare, it)
				}
			}
			slices.SortStableFunc(spare, func(a, b item) int {
				if a.active != b.active {
					if a.active {
						return -1
					}
					return 1
				}
				if c := a.App.StartedAt.Compare(b.App.StartedAt); c != 0 {
					return c
				}
				return a.App.Copy - b.App.Copy
			})
			for _, it := range spare[:min(len(spare), total)] {
				delete(removed, it.App.Copy)
			}
		}

		kept = nil
		for _, it := range items {
			if !removed[it.App.Copy] {
				kept = append(kept, it)
				continue
			}
			pid := 0
			if it.Alive {
				pid = it.App.PID
			}
			plan.Steps = append(plan.Steps, Step{Type: ActionStop, Copy: it.App.Copy, PID: pid})
		}
	}

	// Rule 5.
	var fresh []int
	used := map[int]bool{}
	for _, it := range kept {
		used[it.App.Copy] = true
	}
	for n := 1; len(kept)+len(fresh) < total; n++ {
		if !used[n] {
			fresh = append(fresh, n)
			used[n] = true
		}
	}

	// Rule 6.
	var starts []Step
	for _, n := range fresh {
		starts = append(starts, Step{Type: ActionStart, Copy: n})
	}
	for _, it := range kept {
		switch {
		case it.Alive || it.Pending || it.gaveUp:
		case !it.App.LastExitAt.IsZero() && now.Sub(it.App.LastExitAt) < backoffDelay(it.App.CrashCount):
			starts = append(starts, Step{Type: ActionBackoff, Copy: it.App.Copy})
		default:
			starts = append(starts, Step{Type: ActionStart, Copy: it.App.Copy})
		}
	}

	// Rule 8.
	var replace *Step
	var outdated []item
	for _, it := range kept {
		if it.outdated {
			outdated = append(outdated, it)
			plan.Outdated = append(plan.Outdated, it.App.Copy)
		}
	}
	if len(outdated) > 0 {
		plan.Waiting = slices.Clone(fresh)
		for _, it := range kept {
			if !it.outdated && !(it.Alive && it.Ready) {
				plan.Waiting = append(plan.Waiting, it.App.Copy)
			}
		}
		slices.Sort(plan.Waiting)

		if len(plan.Waiting) == 0 {
			slices.SortStableFunc(outdated, stopCompare)
			first := outdated[0]
			replace = &Step{Type: ActionStart, Copy: first.App.Copy, PID: first.App.PID, Replace: true, Changed: first.changed, Restart: first.restart}
		} else {
			plan.Rolling = true
		}
	}

	// Rule 7.
	var ranked []CopyView
	for _, it := range kept {
		if replace != nil && replace.Copy == it.App.Copy {
			continue
		}
		ranked = append(ranked, it.CopyView)
	}
	plan.Active, plan.Warm = rankRoles(ranked, spec.EffectiveReplicas())

	// Steps: stops (already in), the replacement, then starts by copy.
	slices.SortStableFunc(plan.Steps, func(a, b Step) int { return a.Copy - b.Copy })
	if replace != nil {
		plan.Steps = append(plan.Steps, *replace)
	}
	slices.SortStableFunc(starts, func(a, b Step) int { return a.Copy - b.Copy })
	plan.Steps = append(plan.Steps, starts...)

	return plan
}

// Roles is rule 7 on its own, for rebuilding the load balancer's table
// without deciding anything: of the copies that are alive and ready, the
// oldest R are active and the rest warm.
func Roles(spec manifest.AppSpec, copies []CopyView) (active, warm []int) {
	return rankRoles(copies, spec.EffectiveReplicas())
}

// rankRoles gives the copies that are alive and ready their roles: ordered
// by when they were started, then by copy number, the first replicas are
// active. Nothing about a role is stored, so a daemon that restarts works
// out the same ones.
func rankRoles(copies []CopyView, replicas int) (active, warm []int) {
	var ready []CopyView
	for _, c := range copies {
		if c.Alive && c.Ready {
			ready = append(ready, c)
		}
	}
	slices.SortStableFunc(ready, func(a, b CopyView) int {
		if c := a.App.StartedAt.Compare(b.App.StartedAt); c != 0 {
			return c
		}
		return a.App.Copy - b.App.Copy
	})
	for i, c := range ready {
		if i < replicas {
			active = append(active, c.App.Copy)
		} else {
			warm = append(warm, c.App.Copy)
		}
	}
	return active, warm
}

// stopCompare orders copies by how little it costs to stop them, cheapest
// first: copies with no process, then starting ones, then ready ones. Among
// the ready, the one with the fewest open sessions goes first, so visitors
// are cut off only when there are fewer ready copies than copies holding
// them (the roles are those of the new settings, so a copy about to be
// demoted still counts for what it holds); a warm copy goes before an
// active one, then the newest, then the highest copy number. It is also the
// order in which an outdated copy is chosen for replacement.
func stopCompare(a, b item) int {
	if a.class != b.class {
		return a.class - b.class
	}
	if a.class == 2 {
		if a.Sessions != b.Sessions {
			return a.Sessions - b.Sessions
		}
		if a.active != b.active {
			if !a.active {
				return -1
			}
			return 1
		}
	}
	if c := b.App.StartedAt.Compare(a.App.StartedAt); c != 0 {
		return c
	}
	return b.App.Copy - a.App.Copy
}

// surplusCompare orders copies for scale-down: the same as stopCompare, except
// that the copies that run the previous version go before the ones that run
// the new version. Among the copies with a process that is always so, ready or
// starting. A copy that is dead (crashed, in backoff or given up) counts as
// one of the new version, and goes after the old ones only while no copy of
// the new version is ready: then the old ready copy must be kept to serve and
// the dead new one is the one retried; once a new copy is ready, the new
// version has proved itself and a dead copy goes first, as in any scale-down,
// rather than a ready old one that still adds capacity.
func surplusCompare(newReady bool) func(a, b item) int {
	return func(a, b item) int {
		if a.outdated != b.outdated && (!newReady || (a.class != 0 && b.class != 0)) {
			if a.outdated {
				return -1
			}
			return 1
		}
		return stopCompare(a, b)
	}
}

// Outdated says whether the copy recorded in app runs something other than
// what is wanted now: the manifest fields it was not started from, and
// whether a restart has been asked for since it started.
func Outdated(spec manifest.AppSpec, revision int, app state.AppState) (changed []string, restart bool) {
	return changedFields(app, spec), app.Revision < revision
}

// Describe is the reply to apply and restart, and the line the daemon logs
// when it starts something. before is what was observed, p the plan, and
// settings the manifest fields that changed and take effect without a
// restart (see SettingsChanged).
func Describe(spec manifest.AppSpec, before []CopyView, p Plan, settings []string) string {
	var upgrade, replace *Step
	var fresh, again, stops, backoffs []int
	var oldPID int
	known := map[int]CopyView{}
	for _, v := range before {
		known[v.App.Copy] = v
	}
	for i, s := range p.Steps {
		switch {
		case s.Type == ActionStop && s.Copy == 0:
			oldPID = s.PID
		case s.Type == ActionStop:
			stops = append(stops, s.Copy)
		case s.Type == ActionBackoff:
			backoffs = append(backoffs, s.Copy)
		case s.Upgrade:
			upgrade = &p.Steps[i]
		case s.Replace:
			replace = &p.Steps[i]
		default:
			if _, ok := known[s.Copy]; ok {
				again = append(again, s.Copy)
			} else {
				fresh = append(fresh, s.Copy)
			}
		}
	}

	name, port := spec.AppName, spec.Port
	multi := spec.CopyCount() > 1
	summary := copiesSummary(spec.EffectiveReplicas(), spec.EffectiveWarm())
	changedList := func(fields []string) string {
		return strings.Join(OrderFields(slices.Concat(fields, settings)), ", ")
	}

	switch {
	case upgrade != nil:
		var parts []string
		if multi {
			parts = append(parts, summary)
		}
		if len(settings) > 0 {
			parts = append(parts, "manifest changed: "+changedList(nil))
		}
		parts = append(parts, "moved behind the load balancer", fmt.Sprintf("stopped the previous instance: pid %d", oldPID))
		return fmt.Sprintf("Starting app %q on port %d (%s)", name, port, strings.Join(parts, "; "))

	case replace != nil:
		otherAlive := slices.ContainsFunc(before, func(v CopyView) bool {
			return v.Alive && v.App.Copy != replace.Copy && !slices.Contains(stops, v.App.Copy)
		})
		if !otherAlive {
			var parts []string
			if fields := changedList(replace.Changed); fields != "" {
				parts = append(parts, "manifest changed: "+fields)
			}
			if len(stops) > 0 {
				parts = append(parts, "stopping "+listCopies(stops))
			}
			parts = append(parts, fmt.Sprintf("stopped the previous instance: pid %d", replace.PID))
			return fmt.Sprintf("Starting app %q on port %d (%s)", name, port, strings.Join(parts, "; "))
		}
		msg := fmt.Sprintf("Restarting app %q one copy at a time", name)
		if fields := changedList(replace.Changed); fields != "" {
			msg += " (manifest changed: " + fields + ")"
		}
		order := "first"
		if len(p.Outdated) < countAlive(before) {
			order = "next"
		}
		msg += fmt.Sprintf(": copy %d %s (stopped pid %d)", replace.Copy, order, replace.PID)
		if len(stops) > 0 {
			msg += "; stopping " + listCopies(stops)
		}
		return msg

	case len(fresh)+len(again) > 0 && countAlive(before) == 0 && len(stops) == 0:
		var parts []string
		if multi {
			parts = append(parts, summary)
		}
		if len(settings) > 0 {
			parts = append(parts, "manifest changed: "+changedList(nil))
		}
		msg := fmt.Sprintf("Starting app %q on port %d", name, port)
		if len(parts) > 0 {
			msg += " (" + strings.Join(parts, "; ") + ")"
		}
		return msg

	case len(fresh)+len(again)+len(stops) > 0:
		var parts []string
		if len(settings) > 0 {
			parts = append(parts, "manifest changed: "+changedList(nil))
		}
		if len(fresh) > 0 {
			parts = append(parts, "starting "+listCopies(fresh))
		}
		if len(again) > 0 {
			parts = append(parts, "starting "+listCopies(again)+" again")
		}
		if len(stops) > 0 {
			parts = append(parts, "stopping "+listCopies(stops))
		}
		if len(settings) > 0 {
			return fmt.Sprintf("Updated %q without a restart (%s)", name, strings.Join(parts, "; "))
		}
		return fmt.Sprintf("Updated %q (%s)", name, strings.Join(parts, "; "))

	case len(backoffs) > 0:
		var lines []string
		for _, n := range backoffs {
			crashes := known[n].App.CrashCount
			if multi {
				lines = append(lines, fmt.Sprintf("Backing off restart of %q copy %d after %d crash(es)", name, n, crashes))
			} else {
				lines = append(lines, fmt.Sprintf("Backing off restart of %q after %d crash(es)", name, crashes))
			}
		}
		return strings.Join(lines, "; ")

	case p.Rolling:
		msg := fmt.Sprintf("Restarting app %q one copy at a time", name)
		if len(settings) > 0 {
			msg += " (manifest changed: " + changedList(nil) + ")"
		}
		return msg + fmt.Sprintf(": waiting for %s to be ready", listCopies(p.Waiting))

	case len(settings) > 0:
		return fmt.Sprintf("Updated %q without a restart (manifest changed: %s)", name, strings.Join(settings, ", "))
	}
	return "Nothing to change"
}

func countAlive(views []CopyView) int {
	n := 0
	for _, v := range views {
		if v.Alive {
			n++
		}
	}
	return n
}

// copiesSummary reads "1 copy", "2 copies", "1 copy and 1 warm spare" or
// "2 copies and 3 warm spares".
func copiesSummary(replicas, warm int) string {
	plural := func(n int, one, many string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, one)
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	s := plural(replicas, "copy", "copies")
	if warm > 0 {
		s += " and " + plural(warm, "warm spare", "warm spares")
	}
	return s
}

// listCopies reads "copy 3", "copies 3 and 4" or "copies 2, 4 and 5".
func listCopies(copies []int) string {
	nums := make([]string, len(copies))
	for i, n := range copies {
		nums[i] = fmt.Sprint(n)
	}
	switch len(nums) {
	case 0:
		return "no copies"
	case 1:
		return "copy " + nums[0]
	}
	return "copies " + strings.Join(nums[:len(nums)-1], ", ") + " and " + nums[len(nums)-1]
}
