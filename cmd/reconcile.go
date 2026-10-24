/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// watched holds the PIDs of the processes this daemon has an exit-watcher on:
// started by this daemon run, with the watcher still to record their exit. A
// process that dies has its exit recorded by its watcher, which has to wait
// for the state lock a tick may be holding; a tick that finds the process
// gone before the watcher got there must leave the decision to it, or it
// starts the copy again without the exit ever being counted (a
// restart-policy never copy would come back). A process adopted from an
// earlier daemon has no watcher, and the tick records its exit itself.
var watched sync.Map

func isWatched(pid int) bool {
	_, ok := watched.Load(pid)
	return ok
}

// seen is the process of each copy that this daemon has found alive, by
// state file and copy: one entry per copy, so it can't grow past the number
// of copies, and dropped when the copy or the app is forgotten. A process
// that is gone and was never seen alive here died while no daemon was
// looking (a reboot, or a daemon that was down), which is not a crash to
// count: the copy just starts, as it always has. Only a process seen alive
// and found dead later, with nobody having recorded its exit, is an unseen
// exit. The started-at time is part of the value so a reused PID can't match
// a different record.
var seen struct {
	sync.Mutex
	m map[seenKey]seenProcess
}

type seenKey struct {
	stateFile string
	copy      int
}

type seenProcess struct {
	pid     int
	started time.Time
}

func markSeen(stateFile string, app state.AppState) {
	seen.Lock()
	defer seen.Unlock()
	if seen.m == nil {
		seen.m = make(map[seenKey]seenProcess)
	}
	seen.m[seenKey{stateFile, app.Copy}] = seenProcess{app.PID, app.StartedAt}
}

func wasSeen(stateFile string, app state.AppState) bool {
	seen.Lock()
	defer seen.Unlock()
	return seen.m[seenKey{stateFile, app.Copy}] == seenProcess{app.PID, app.StartedAt}
}

// forgetSeen drops the entry of one copy: its exit has been counted, or it
// is gone.
func forgetSeen(stateFile string, copy int) {
	seen.Lock()
	defer seen.Unlock()
	delete(seen.m, seenKey{stateFile, copy})
}

// forgetApp drops the entries of every copy of the app at stateFile.
func forgetApp(stateFile string) {
	seen.Lock()
	defer seen.Unlock()
	for key := range seen.m {
		if key.stateFile == stateFile {
			delete(seen.m, key)
		}
	}
}

// notStarted holds, by state file, why an app's copies aren't started: the
// port the load balancer would listen on is taken. That is not a crash of
// the app, so nothing is counted against it; the reason is shown by status,
// logged once when it changes, and tried again on every tick.
var notStarted sync.Map

// setNotStarted records why spec's app could not start, logging it when it
// differs from the last reason.
func setNotStarted(spec manifest.AppSpec, err error) {
	reason := err.Error()
	if prev, ok := notStarted.Swap(spec.AppStateFile, reason); !ok || prev != reason {
		log.Printf("reconcile: %q: not started: %v", spec.AppName, err)
	}
}

// notStartedError is an error that setNotStarted has already logged, so the
// caller that gets it back doesn't log it again on every tick.
type notStartedError struct{ error }

func (e notStartedError) Unwrap() error { return e.error }

func clearNotStarted(stateFile string) {
	notStarted.Delete(stateFile)
}

func notStartedReason(stateFile string) string {
	reason, _ := notStarted.Load(stateFile)
	s, _ := reason.(string)
	return s
}

// reconcileOpts says why a cycle runs.
type reconcileOpts struct {
	// explicit is an apply or a restart: "run it now". Every copy's crash
	// history is cleared, and an exit nobody has recorded yet is not waited
	// for.
	explicit bool

	// settings names manifest fields that changed and take effect without a
	// restart (see reconcile.SettingsChanged); they only appear in the reply.
	settings []string
}

// observeMode says how much of a cycle's side effects an observation has.
type observeMode int

const (
	observeTick     observeMode = iota // leave exits to their watchers, count unseen ones
	observeExplicit                    // neither
	observeReadOnly                    // also mark nothing seen and change nothing
)

// dialOf is the address the load balancer dials for a copy.
func dialOf(app state.AppState) string {
	return fmt.Sprintf("%s:%d", process.InternalHost, app.Port)
}

// observe looks at every copy recorded in st. The state is changed (and the
// result true) only for what a cycle records itself: an exit nobody saw, and
// a dead record from before the load balancer, which becomes copy 1 again.
// The caller saves st then.
func observe(spec manifest.AppSpec, st *state.State, mode observeMode) (views []reconcile.CopyView, changed bool) {
	recs := st.Copies()
	alive := process.AliveAll(spec, recs)

	var sessions map[string]int
	if slices.Contains(alive, true) {
		sessions = gateway.Sessions()
	}

	for i, rec := range recs {
		v := reconcile.CopyView{Alive: alive[i]}

		switch {
		case alive[i]:
			if mode != observeReadOnly {
				markSeen(spec.AppStateFile, rec)
			}
			if rec.Copy >= 1 {
				v.Ready = currentHooks().ObserveProbe(process.InternalHost, rec.Port)
				v.Sessions = sessions[dialOf(rec)]
			} else {
				v.Ready = process.IsReady(spec.EffectiveHost(), rec.Port)
			}

		case rec.Copy == 0:
			// A record from before the load balancer whose process is gone:
			// its port was the app's own. It is copy 1 again, with the crash
			// history it has and an internal port still to be chosen.
			st.Remove(0)
			rec.Copy, rec.Port = 1, 0
			st.Put(rec)
			changed = true

		case mode == observeTick && rec.PID > 0:
			if isWatched(rec.PID) {
				v.Pending = true // the watcher records the exit and reconciles
			} else if !rec.StartedAt.IsZero() && rec.LastExitAt.Before(rec.StartedAt) && wasSeen(spec.AppStateFile, rec) {
				// This daemon saw the process alive, nobody saw it exit and
				// nobody will: count it, as a failure since its status is
				// unknown.
				forgetSeen(spec.AppStateFile, rec.Copy)
				log.Printf("reconcile: %q: copy %d (pid %d) is gone and its exit was not seen; counting it as a failure", spec.AppName, rec.Copy, rec.PID)
				rec = reconcile.RecordExit(rec, process.Exit{At: time.Now(), Failed: true})
				st.Put(rec)
				changed = true
			}
		}

		v.App = rec
		views = append(views, v)
	}
	return views, changed
}

// poolOf is the copies plan says should serve, as the load balancer dials
// them.
func poolOf(plan reconcile.Plan, st state.State) proxy.Pool {
	dials := func(copies []int) []string {
		var out []string
		for _, n := range copies {
			if rec, ok := st.Copy(n); ok {
				out = append(out, dialOf(rec))
			}
		}
		return out
	}
	return proxy.Pool{Active: dials(plan.Active), Warm: dials(plan.Warm)}
}

// reconcileLocked runs one decide-and-act cycle for spec: load state, look at
// every copy, decide what each needs, tell the load balancer who serves, then
// stop and start processes. The caller holds the state lock, held across the
// whole cycle, through process.Start: an apply and a tick deciding to start
// off the same loaded state would otherwise both launch, and the loser's
// port check failure would be recorded as a phantom crash. The callers are
// handleApply, which takes the lock before it updates the registry;
// handleRestart, which holds it across its revision bump; and
// triggerReconcile, which takes it first so it can read the registry's
// current spec under it. The exit-watchers, the ready-watchers and the
// ticker reach it through triggerReconcile.
//
// The load balancer is told first. A copy that died leaves the pool and a
// warm one takes over before the slow parts (waiting for a port, stopping a
// process for up to 12 seconds), and a copy about to be stopped gets no new
// visitors.
func reconcileLocked(spec manifest.AppSpec, o reconcileOpts) (api.ApplyResponse, error) {
	st, err := state.Load(spec.AppStateFile)
	if err != nil {
		return api.ApplyResponse{}, err
	}

	// Clear the whole exit record, not just CrashCount: DecideCopies gates the
	// restart policy on LastExitAt, so leaving it set would let a `never`
	// copy stay given-up through an explicit apply.
	dirty := false
	if o.explicit {
		for _, rec := range st.Copies() {
			if rec.CrashCount != 0 || !rec.LastExitAt.IsZero() || rec.LastExitFailed {
				rec.CrashCount, rec.LastExitAt, rec.LastExitFailed = 0, time.Time{}, false
				st.Put(rec)
				dirty = true
			}
		}
	}

	// An explicit apply or restart means "run it now" and takes none of the
	// tick's care for exits.
	mode := observeTick
	if o.explicit {
		mode = observeExplicit
	}
	views, changed := observe(spec, &st, mode)
	dirty = dirty || changed

	plan := reconcile.DecideCopies(spec, st.Revision, views, time.Now())
	if dirty {
		if err := state.Save(spec.AppStateFile, st); err != nil {
			return api.ApplyResponse{}, err
		}
	}

	resp := api.ApplyResponse{Action: plan.Action(), Message: reconcile.Describe(spec, views, plan, o.settings)}

	// Who serves, before anything is stopped or started.
	pool := poolOf(plan, st)
	if cur, ok := gateway.Pool(spec.AppStateFile); !ok || !samePool(cur, pool) {
		log.Printf("reconcile: %q: %s", spec.AppName, reconcile.DescribeRoles(plan.Active, plan.Warm))
	}
	// A failure is logged by the gateway, once; the table is sent again with
	// the next change, and whenever the load balancer comes up.
	gateway.SetPool(spec.AppStateFile, pool)

	if err := execute(spec, &st, plan); err != nil {
		return api.ApplyResponse{}, err
	}

	// A copy that is alive but not ready yet is promoted as soon as it is,
	// including one an earlier daemon started.
	for _, v := range views {
		if v.Alive && !v.Ready && v.App.Copy >= 1 {
			go watchReady(spec.AppStateFile, v.App.PID, v.App.Port)
		}
	}
	return resp, nil
}

func samePool(a, b proxy.Pool) bool {
	return proxy.Table{"": a}.Equal(proxy.Table{"": b})
}

// execute carries out plan's steps, in order: stop what is surplus (and the
// process an upgrade replaces), check the port, replace an outdated copy,
// start the rest. st is the state the plan was made from; it is saved as it
// changes.
func execute(spec manifest.AppSpec, st *state.State, plan reconcile.Plan) error {
	var stops, starts []reconcile.Step
	for _, step := range plan.Steps {
		switch step.Type {
		case reconcile.ActionStop:
			stops = append(stops, step)
		case reconcile.ActionStart:
			starts = append(starts, step)
		}
	}

	if err := stopSurplus(spec, st, stops); err != nil {
		return err
	}
	if len(starts) == 0 {
		return nil
	}

	// Checked here, not in manifest.Validate: by this point our own
	// last-recorded copies (if any) are either dead, per DecideCopies, or
	// were just stopped above, so a port that's occupied right now genuinely
	// belongs to something else — Validate has no state to make that
	// distinction and would wrongly reject every routine re-apply of an app
	// that's correctly, already running on its own port. claimPort also knows
	// which ports the load balancer itself holds, which a bind check would
	// trip over. It comes before any internal port is chosen so that a
	// refused port leaves the app out of the load balancer's settings, and
	// before an outdated copy is stopped so that the app keeps serving. Not
	// being able to start is not a crash: nothing new ran, so none is
	// recorded.
	if err := claimPort(spec); err != nil {
		setNotStarted(spec, err)
		return notStartedError{err}
	}

	for _, step := range starts {
		if step.Replace {
			if err := replaceCopy(spec, st, step); err != nil {
				return err
			}
		}
		if err := launchCopy(spec, st, step.Copy); err != nil {
			return err
		}
	}
	clearNotStarted(spec.AppStateFile)
	return nil
}

// stopSurplus stops the processes of the stop steps in parallel and drops
// their records: the copies a smaller manifest no longer wants, and the
// process of an app from before the load balancer, which holds the app's own
// port. Stop re-checks identity before each signal, so a PID that changed
// hands since the observation is left alone. A copy that fails to stop keeps
// its record; the others are removed.
func stopSurplus(spec manifest.AppSpec, st *state.State, stops []reconcile.Step) error {
	if len(stops) == 0 {
		return nil
	}

	errs := make([]error, len(stops))
	var wg sync.WaitGroup
	for i, step := range stops {
		rec, _ := st.Copy(step.Copy)
		if step.PID == 0 {
			continue // no process: only the record goes
		}
		if step.Copy == 0 {
			log.Printf("reconcile: %q: moving behind the load balancer: stopping the previous instance (pid %d), which holds port %d itself", spec.AppName, rec.PID, rec.Port)
		} else {
			log.Printf("reconcile: %q: stopping copy %d (pid %d): fewer copies wanted", spec.AppName, step.Copy, rec.PID)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = process.Stop(rec, identityFallbacks(spec)...)
		}()
	}
	wg.Wait()

	var first error
	for i, step := range stops {
		if errs[i] != nil {
			if first == nil {
				first = fmt.Errorf("stopping copy %d of %q (pid %d): %w", step.Copy, spec.AppName, step.PID, errs[i])
			}
			continue
		}
		st.Remove(step.Copy)
		forgetSeen(spec.AppStateFile, step.Copy)
	}
	if err := state.Save(spec.AppStateFile, *st); err != nil {
		return err
	}
	return first
}

// replaceCopy stops the running process of an outdated copy and clears its
// record down to the crash history and the internal port, so the replacement
// takes the port over: the load balancer already points there and the restart
// needs no reload. Any failure to start is recorded on this cleared record,
// and the stopped process's exit-watcher sees a PID that isn't its own and
// stays quiet. A failed Stop records no crash: nothing new ran.
func replaceCopy(spec manifest.AppSpec, st *state.State, step reconcile.Step) error {
	rec, ok := st.Copy(step.Copy)
	if !ok {
		return nil
	}
	switch {
	case len(step.Changed) > 0:
		log.Printf("reconcile: %q: restarting copy %d (pid %d) (manifest changed: %s)", spec.AppName, step.Copy, rec.PID, strings.Join(step.Changed, ", "))
	default:
		log.Printf("reconcile: %q: restarting copy %d (pid %d) for a restart", spec.AppName, step.Copy, rec.PID)
	}
	if _, err := process.Stop(rec, identityFallbacks(spec)...); err != nil {
		return fmt.Errorf("stopping the previous instance of %q (copy %d, pid %d): %w", spec.AppName, step.Copy, rec.PID, err)
	}
	forgetSeen(spec.AppStateFile, step.Copy)

	st.Put(state.AppState{Copy: rec.Copy, Port: rec.Port, CrashCount: rec.CrashCount, LastExitAt: rec.LastExitAt, LastExitFailed: rec.LastExitFailed})
	return state.Save(spec.AppStateFile, *st)
}

// launchCopy launches copy n of spec's app at the state's revision and
// records it, with an exit-watcher and a ready-watcher.
func launchCopy(spec manifest.AppSpec, st *state.State, n int) error {
	if d := currentHooks().StartDelay; d > 0 {
		time.Sleep(d)
	}

	port, err := copyPort(spec, st, n)
	if err != nil {
		setNotStarted(spec, err)
		return notStartedError{err}
	}

	newApp, done, err := process.Start(spec, n, port, st.Revision)
	if err != nil {
		// A failed exec() is a crash too — it just never produces an
		// exit-channel event, since there was never a process to
		// Wait() on. Without this, a bad entrypoint/missing Rscript
		// would retry every tick forever with no backoff, exactly
		// like the bug this whole mechanism exists to prevent.
		recordExitLocked(spec, n, process.Exit{At: time.Now(), Failed: true})
		return err
	}

	// Carry crash history forward across the restart itself — it's
	// only RecordExit, below, that ever changes it. Without this a
	// successful restart would silently wipe the crash count the
	// backoff logic above just relied on.
	if old, ok := st.Copy(n); ok {
		newApp.CrashCount = old.CrashCount
		newApp.LastExitAt = old.LastExitAt
		newApp.LastExitFailed = old.LastExitFailed
	}
	st.Put(newApp)
	if err := state.Save(spec.AppStateFile, *st); err != nil {
		return err
	}

	// React to this specific process dying immediately, instead of
	// waiting for the next reconcile tick (up to reconcileInterval
	// later). Only this PID's exit counts: if a replacement has since
	// taken over the record, recordExit refuses it and nothing is
	// reconciled. The reconcile itself uses the registry's current
	// spec, not the one captured here, which may predate a re-apply.
	pid := newApp.PID
	watched.Store(pid, struct{}{})
	go func() {
		exit := <-done
		recorded := recordExit(spec, n, pid, exit)
		watched.Delete(pid)
		if recorded {
			triggerReconcile(spec.AppStateFile)
		}
	}()
	go watchReady(spec.AppStateFile, pid, port)
	return nil
}

// recordExit persists exit into spec's state.json ahead of the
// triggerReconcile call that follows it, so DecideCopies sees up-to-date crash
// history on the very next reconcile pass. Kept as its own disk
// read-modify-write rather than folded into reconcileLocked's, since it has
// to run unconditionally on every exit, independent of whatever the next
// reconcile pass decides on. Takes the state lock itself — for
// the exit-watcher goroutine, which holds nothing.
//
// An exit caused by `stop` or `delete` is not a crash: both mark the app
// unmanaged before signaling it, so by the time this runs for that exit
// there's nothing to record — writing one would show as a phantom
// `CRASHES 1` in status, or (for delete) recreate the state file that was
// just removed.
//
// n and pid are the copy and the process that exited. The exit is recorded
// only if state.json still names that PID for that copy: a record that a
// re-apply, restart or scale-down replaced, cleared or removed must not take
// the old process's exit, or it shows up as a phantom crash against the
// replacement. The result says whether the exit was recorded; callers
// react (reconcile) only when it was.
func recordExit(spec manifest.AppSpec, n, pid int, exit process.Exit) bool {
	if !appRegistry.Managed(spec.AppStateFile) {
		return false
	}

	unlock := state.Lock(spec.AppStateFile)
	defer unlock()

	current, err := state.Load(spec.AppStateFile)
	if err != nil {
		log.Printf("reconcile: %q: recording exit: %v", spec.AppName, err)
		return false
	}
	if rec, ok := current.Copy(n); !ok || rec.PID != pid {
		return false
	}

	recordExitLocked(spec, n, exit)
	return true
}

// recordExitLocked is recordExit for a caller that already holds the state
// lock — launchCopy, recording a Start failure mid-cycle.
func recordExitLocked(spec manifest.AppSpec, n int, exit process.Exit) {
	current, err := state.Load(spec.AppStateFile)
	if err != nil {
		log.Printf("reconcile: %q: recording exit: %v", spec.AppName, err)
		return
	}

	rec, _ := current.Copy(n)
	rec.Copy = n
	current.Put(reconcile.RecordExit(rec, exit))

	if err := state.Save(spec.AppStateFile, current); err != nil {
		log.Printf("reconcile: %q: recording exit: %v", spec.AppName, err)
	}
}
