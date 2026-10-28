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
	App     state.AppState
	Alive   bool // running, and identifiably this copy (entrypoint and port)
	Ready   bool // alive and accepting connections on its internal port
	Pending bool // not alive, and its exit-watcher has yet to record the exit

	// Sessions is the number of requests open to the copy through the load
	// balancer (a Shiny session is one open WebSocket). SessionsKnown says the
	// load balancer answered: without it, Sessions is 0 but means nothing, and
	// no copy is taken to have no visitors left.
	Sessions      int
	SessionsKnown bool

	// PortTaken says the socket that listens on the copy's port belongs to
	// another program: the copy is not ready and never will be on that port.
	// The caller moves it before deciding; DecideCopies doesn't look at it.
	PortTaken bool
}

// StopReason says why a Stop step stops a copy.
type StopReason string

const (
	StopSurplus  StopReason = "surplus"  // fewer copies wanted, and this one isn't ready, or must not drain
	StopUpgrade  StopReason = "upgrade"  // the process from before the load balancer, which holds the app's port
	StopOutdated StopReason = "outdated" // runs a previous version and isn't ready: it never served anyone
	StopDrained  StopReason = "drained"  // draining, and no sessions are left
	StopIdle     StopReason = "idle"     // an old spare with no sessions at the switch: it has no visitors to wait for
	StopTimeout  StopReason = "timeout"  // draining for drain-timeout
	StopNoDrain  StopReason = "no-drain" // would drain, but --now or drain-timeout 0 says stop
	StopTooMany  StopReason = "too-many" // more copies with visitors draining than the app runs
	StopGone     StopReason = "gone"     // no process: only the record goes
)

// Step is one thing DecideCopies wants done.
type Step struct {
	Type     ActionType // ActionStart, ActionStop or ActionBackoff
	Copy     int        // the copy number; 0 only for a Stop of a record from before the load balancer
	PID      int        // Stop: the live process to stop; 0 when there is none
	Why      StopReason // Stop: why
	Sessions int        // Stop: the sessions open when it was decided, for the log
	Upgrade  bool       // Start of copy 1 replacing a process from before the load balancer
}

// Plan is DecideCopies' answer.
type Plan struct {
	Steps []Step // Stops, then Starts and Backoffs, each in copy order

	// Drain lists the copies that start draining now: the caller records
	// draining-since on them.
	Drain []int

	Active   []int // copies new visitors go to, in role order
	Warm     []int // ready spares, in role order
	Draining []int // copies that keep their visitors and get no new ones, longest draining first

	// Rolling says copies of a previous version serve while the copies of the
	// new version are not ready (Waiting); Outdated names those old copies.
	// Switched says this plan hands new visitors over to the new version.
	Rolling  bool
	Switched bool
	Waiting  []int
	Outdated []int

	// Changed and Restart say why the old copies are outdated (manifest
	// fields, a restart); OldStop says they stop instead of draining once
	// the new ones are ready (--now, or drain-timeout 0).
	Changed []string
	Restart bool
	OldStop bool
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
	if backoff && len(p.Drain) == 0 && !slices.ContainsFunc(p.Steps, func(s Step) bool { return s.Type == ActionStop }) {
		return ActionBackoff
	}
	return ActionNone
}

// DrainSettle is how long a draining copy is kept at least, even with no
// sessions: a page whose HTML came from the copy just before it started
// draining opens its WebSocket a moment later, and that session belongs on the
// same copy.
const DrainSettle = 10 * time.Second

// Input is what DecideCopies decides from.
type Input struct {
	Revision  int        // State.Revision
	Copies    []CopyView // every recorded copy
	Now       time.Time  // the caller's clock, passed in so this stays deterministic
	SkipDrain bool       // the request asked for --now: what would drain stops instead
	Settle    time.Duration
}

// item is a copy with what DecideCopies works out about it.
type item struct {
	CopyView
	outdated bool
	changed  []string
	restart  bool
	gaveUp   bool
	class    int  // 0 no process, 1 starting, 2 ready; see stopCompare
	active   bool // ready, and among the first R by started-at of its group
}

// DecideCopies is the reconcile loop's pure decision function: given what
// spec wants and what was observed of each recorded copy, it says what to do,
// never does it itself.
//
// The rules, in order:
//  1. A live record without a copy number is a process from before the load
//     balancer, holding the app's port itself. It is stopped and the app
//     starts again behind the load balancer, with every copy.
//  2. Each copy is draining (it has draining-since), old (alive, not draining,
//     and started from an older revision or another manifest), or current
//     (anything else: up to date, starting, dead or pending).
//  3. A draining copy is never started again and never gets a role but
//     draining. It stops when it has no process, when it must not drain
//     (--now, drain-timeout 0), when drain-timeout has passed since it began,
//     or when the load balancer says it has no sessions and it has drained
//     for at least the settle time.
//  4. An old copy that is not ready, and has no sessions by the load
//     balancer's count, is stopped: it never served and never will.
//  5. Current copies are kept at replicas + warm-replicas: the surplus goes in
//     stopCompare's order, a ready one by draining (or stopping, when it must
//     not drain), the others stopped; missing ones start under the lowest copy
//     numbers no record has; a dead one starts again in place after its
//     backoff, unless the restart policy has given up on it. While old copies
//     still serve (before the switch) only replicas current copies are
//     wanted: the new version starts with the copies that serve, its spares
//     start once it has taken over, so a restart runs few processes at once.
//  6. Old copies beyond replicas + warm-replicas drain too (a scale-down in
//     the middle of a rollout, or a restart that overtakes a rollout whose new
//     copy is ready but has not switched yet); one that is an idle spare stops
//     at once instead, as at the switch.
//  7. The switch: once replicas current copies are ready, every old copy
//     drains (or stops, when it must not drain). An old spare that has no
//     sessions by the load balancer's count stops at once instead: it takes no
//     visitors unless it stands in for a dead active copy, and then it has
//     sessions, so only the active copies wait out the settle time. Until the
//     switch the old copies keep serving and the rollout waits; a new version
//     that never gets ready leaves them serving.
//  8. Roles. Before the switch the old ready copies come first, then the
//     current ready ones, each by started-at: the first replicas are active,
//     the rest warm. Otherwise the current ready copies by started-at. A
//     restarted copy is the newest, so it comes back as a spare and roles
//     never swap back on their own.
//  9. At most replicas + warm-replicas copies with visitors drain at once:
//     beyond that, the ones draining longest stop.
func DecideCopies(spec manifest.AppSpec, in Input) Plan {
	total := spec.CopyCount()
	replicas := spec.EffectiveReplicas()
	timeout := spec.EffectiveDrainTimeout()
	now := in.Now

	// Rule 1.
	for _, c := range in.Copies {
		if c.App.Copy == 0 && c.Alive {
			steps := []Step{
				{Type: ActionStop, Copy: 0, PID: c.App.PID, Why: StopUpgrade},
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
	views := slices.Clone(in.Copies)
	for i := range views {
		if views[i].App.Copy == 0 {
			views[i].App.Copy, views[i].App.Port = 1, 0
		}
	}
	slices.SortStableFunc(views, func(a, b CopyView) int { return a.App.Copy - b.App.Copy })

	used := map[int]bool{}
	for _, v := range views {
		used[v.App.Copy] = true
	}

	var plan Plan
	var stops, starts []Step
	stop := func(it item, why StopReason) {
		pid := 0
		if it.Alive {
			pid = it.App.PID
		}
		stops = append(stops, Step{Type: ActionStop, Copy: it.App.Copy, PID: pid, Why: why, Sessions: it.Sessions})
	}
	mustNotDrain := func(it item) bool { return in.SkipDrain || it.App.SkipDrain || timeout == 0 }
	// drainOrStop retires a ready copy: it drains, unless it must not.
	var drained []item
	drainOrStop := func(it item, why StopReason) {
		if mustNotDrain(it) {
			stop(it, why)
			return
		}
		drained = append(drained, it)
	}

	// Rule 2.
	var draining, old, current []item
	for _, v := range views {
		it := item{CopyView: v}
		switch {
		case !v.Alive:
			it.class = 0
		case !v.Ready:
			it.class = 1
		default:
			it.class = 2
		}
		switch {
		case !v.App.DrainingSince.IsZero():
			draining = append(draining, it)
		case v.Alive:
			it.changed, it.restart = Outdated(spec, in.Revision, v.App)
			it.outdated = len(it.changed) > 0 || it.restart
			if it.outdated {
				old = append(old, it)
			} else {
				current = append(current, it)
			}
		default:
			if !v.Pending {
				it.gaveUp = GaveUp(spec, v.App)
			}
			current = append(current, it)
		}
	}

	// Rule 3.
	var stillDraining []item
	for _, d := range draining {
		age := now.Sub(d.App.DrainingSince)
		switch {
		case !d.Alive:
			stop(d, StopGone)
		case mustNotDrain(d):
			stop(d, StopNoDrain)
		case age >= timeout:
			stop(d, StopTimeout)
		case d.SessionsKnown && d.Sessions == 0 && age >= in.Settle:
			stop(d, StopDrained)
		default:
			stillDraining = append(stillDraining, d)
		}
	}

	// Rule 4.
	var oldLive []item
	for _, o := range old {
		if !o.Ready && o.SessionsKnown && o.Sessions == 0 {
			stop(o, StopOutdated)
			continue
		}
		oldLive = append(oldLive, o)
	}
	for _, o := range oldLive {
		plan.Changed = append(plan.Changed, o.changed...)
		plan.Restart = plan.Restart || o.restart
	}
	plan.Changed = OrderFields(dedupe(plan.Changed))

	// Rule 5.
	markActive(current, replicas)
	kept := current
	if len(current) > total {
		order := slices.Clone(current)
		slices.SortStableFunc(order, stopCompare)
		removed := map[int]bool{}
		for _, it := range order[:len(current)-total] {
			removed[it.App.Copy] = true
			if it.class == 2 {
				drainOrStop(it, StopSurplus)
			} else {
				stop(it, StopSurplus)
			}
		}
		kept = nil
		for _, it := range current {
			if !removed[it.App.Copy] {
				kept = append(kept, it)
			}
		}
	}

	// Rule 6.
	var oldServing, oldWaiting []item
	for _, o := range oldLive {
		if o.class == 2 {
			oldServing = append(oldServing, o)
		} else {
			oldWaiting = append(oldWaiting, o)
		}
	}
	markActive(oldServing, replicas)
	if len(oldServing) > total {
		order := slices.Clone(oldServing)
		slices.SortStableFunc(order, stopCompare)
		extra := map[int]bool{}
		for _, it := range order[:len(oldServing)-total] {
			extra[it.App.Copy] = true
			if idleSpare(it) {
				stop(it, StopIdle)
				continue
			}
			drainOrStop(it, StopSurplus)
		}
		oldServing = slices.DeleteFunc(oldServing, func(it item) bool { return extra[it.App.Copy] })
	}

	// Rule 7.
	var currentReady []CopyView
	for _, it := range kept {
		if it.Alive && it.Ready {
			currentReady = append(currentReady, it.CopyView)
		}
	}
	plan.OldStop = in.SkipDrain || timeout == 0
	if len(oldServing)+len(oldWaiting) > 0 && len(currentReady) >= replicas {
		plan.Switched = true
		for _, o := range slices.Concat(oldServing, oldWaiting) {
			if idleSpare(o) {
				stop(o, StopIdle)
				continue
			}
			drainOrStop(o, StopNoDrain)
		}
		oldServing, oldWaiting = nil, nil
	}
	rolling := len(oldServing)+len(oldWaiting) > 0

	// Rule 5, the starts. While old copies still serve, only replicas copies
	// of the new version are wanted; its spares start once it has taken over.
	want := total
	if rolling {
		want = replicas
	}
	var fresh []int
	for n := 1; len(kept)+len(fresh) < want; n++ {
		if !used[n] {
			fresh = append(fresh, n)
			used[n] = true
		}
	}
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
	if rolling {
		plan.Rolling = true
		ranked := slices.Concat(byStart(views0(oldServing)), byStart(currentReady))
		for i, v := range ranked {
			if i < replicas {
				plan.Active = append(plan.Active, v.App.Copy)
			} else {
				plan.Warm = append(plan.Warm, v.App.Copy)
			}
		}
		plan.Waiting = slices.Clone(fresh)
		for _, it := range kept {
			if !(it.Alive && it.Ready) {
				plan.Waiting = append(plan.Waiting, it.App.Copy)
			}
		}
		slices.Sort(plan.Waiting)
		allSkip := true
		for _, o := range slices.Concat(oldServing, oldWaiting) {
			plan.Outdated = append(plan.Outdated, o.App.Copy)
			allSkip = allSkip && o.App.SkipDrain
		}
		slices.Sort(plan.Outdated)
		plan.OldStop = plan.OldStop || allSkip
	} else {
		plan.Active, plan.Warm = rankRoles(currentReady, replicas)
	}

	// Rule 9, and the draining list: the copies still draining, then the ones
	// that start now, longest draining first.
	type drainer struct {
		item
		since time.Time
		fresh bool
	}
	var all []drainer
	for _, d := range stillDraining {
		all = append(all, drainer{d, d.App.DrainingSince, false})
	}
	for _, d := range drained {
		all = append(all, drainer{d, now, true})
	}
	slices.SortStableFunc(all, func(a, b drainer) int {
		if c := a.since.Compare(b.since); c != 0 {
			return c
		}
		return a.App.Copy - b.App.Copy
	})
	busy := 0
	for _, d := range all {
		if !(d.SessionsKnown && d.Sessions == 0) {
			busy++
		}
	}
	// The copies that stop for the cap: the longest draining first and, among
	// drains that began together, a spare before an active copy, which is the
	// one most likely to hold the visitors.
	byCut := slices.Clone(all)
	slices.SortStableFunc(byCut, func(a, b drainer) int {
		if c := a.since.Compare(b.since); c != 0 {
			return c
		}
		if a.active != b.active {
			if !a.active {
				return -1
			}
			return 1
		}
		return a.App.Copy - b.App.Copy
	})
	cut := map[int]bool{}
	for _, d := range byCut {
		if busy <= total {
			break
		}
		if d.SessionsKnown && d.Sessions == 0 {
			continue
		}
		cut[d.App.Copy] = true
		stop(d.item, StopTooMany)
		busy--
	}
	for _, d := range all {
		if cut[d.App.Copy] {
			continue
		}
		plan.Draining = append(plan.Draining, d.App.Copy)
		if d.fresh {
			plan.Drain = append(plan.Drain, d.App.Copy)
		}
	}
	slices.Sort(plan.Drain)

	slices.SortStableFunc(stops, func(a, b Step) int { return a.Copy - b.Copy })
	slices.SortStableFunc(starts, func(a, b Step) int { return a.Copy - b.Copy })
	plan.Steps = append(stops, starts...)
	return plan
}

// idleSpare says a retired old copy has no visitors to wait for: it is ready,
// it is not an active copy (a spare takes none unless it stood in, and then it
// has sessions) and the load balancer says it has none. It stops at once, with
// no settle, so a redeploy keeps few processes alive.
func idleSpare(it item) bool {
	return it.class == 2 && !it.active && it.SessionsKnown && it.Sessions == 0
}

func dedupe(s []string) []string {
	var out []string
	for _, x := range s {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func views0(items []item) []CopyView {
	out := make([]CopyView, len(items))
	for i, it := range items {
		out[i] = it.CopyView
	}
	return out
}

// byStart sorts copies by started-at, then copy number.
func byStart(vs []CopyView) []CopyView {
	out := slices.Clone(vs)
	slices.SortStableFunc(out, func(a, b CopyView) int {
		if c := a.App.StartedAt.Compare(b.App.StartedAt); c != 0 {
			return c
		}
		return a.App.Copy - b.App.Copy
	})
	return out
}

// markActive flags the ready items that would be active among them.
func markActive(items []item, replicas int) {
	vs := views0(items)
	active, _ := rankRoles(vs, replicas)
	for i := range items {
		items[i].active = items[i].class == 2 && slices.Contains(active, items[i].App.Copy)
	}
}

// Roles is DecideCopies' roles alone, for rebuilding the load balancer's table
// without doing anything.
func Roles(spec manifest.AppSpec, in Input) (active, warm, draining []int) {
	p := DecideCopies(spec, in)
	return p.Active, p.Warm, p.Draining
}

// rankRoles gives the copies that are alive and ready their roles: ordered
// by when they were started, then by copy number, the first replicas are
// active. Nothing about a role is stored, so a daemon that restarts works
// out the same ones.
func rankRoles(copies []CopyView, replicas int) (active, warm []int) {
	var ready []CopyView
	for _, c := range copies {
		if c.Alive && c.Ready && c.App.Copy >= 1 {
			ready = append(ready, c)
		}
	}
	for i, c := range byStart(ready) {
		if i < replicas {
			active = append(active, c.App.Copy)
		} else {
			warm = append(warm, c.App.Copy)
		}
	}
	return active, warm
}

// stopCompare orders copies by how little it costs to retire them, cheapest
// first: copies with no process, then starting ones, then ready ones. Among
// the ready, the one with the fewest open sessions goes first; a warm copy
// goes before an active one, then the newest, then the highest copy number.
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

// Outdated says whether the copy recorded in app runs something other than
// what is wanted now: the manifest fields it was not started from, and
// whether a restart has been asked for since it started.
func Outdated(spec manifest.AppSpec, revision int, app state.AppState) (changed []string, restart bool) {
	return changedFields(app, spec), app.Revision < revision
}

// FormatDuration writes d the way a manifest would: "15m", "90s" as "1m30s",
// "1h", "1h30m".
func FormatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
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

// DescribeRoles says who serves visitors, for the log line written when the
// roles change: "copy 1 serves visitors", "copies 2 and 3 serve visitors;
// copy 1 is warm; copy 4 is draining", or "no copy serves visitors yet".
func DescribeRoles(active, warm, draining []int) string {
	msg := "no copy serves visitors yet"
	if len(active) > 0 {
		msg = ListCopies(active) + " serve"
		if len(active) == 1 {
			msg += "s"
		}
		msg += " visitors"
	}
	is := func(l []int) string {
		if len(l) == 1 {
			return " is "
		}
		return " are "
	}
	if len(warm) > 0 {
		msg += "; " + ListCopies(warm) + is(warm) + "warm"
	}
	if len(draining) > 0 {
		msg += "; " + ListCopies(draining) + is(draining) + "draining"
	}
	return msg
}

// ListCopies reads "copy 3", "copies 3 and 4" or "copies 2, 4 and 5".
func ListCopies(copies []int) string {
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
