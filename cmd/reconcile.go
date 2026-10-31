/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

// This file is the cycle: observing the copies of an app, deciding, recording,
// and carrying out the plan (starting, stopping, recording exits). Its
// neighbours hold the exit-watchers (reconcile_watch.go) and what is done
// about ports (reconcile_ports.go).

import (
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// reconcileOpts says why a cycle runs.
type reconcileOpts struct {
	// explicit is an apply or a restart: "run it now". Every copy's crash
	// history is cleared, and an exit nobody has recorded yet is not waited
	// for.
	explicit bool

	// settings names manifest fields that changed and take effect without a
	// restart (see reconcile.SettingsChanged); they only appear in the reply.
	settings []string

	// skipDrain is --now: what is replaced or no longer wanted stops instead
	// of draining, and drains in progress end.
	skipDrain bool
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
		sessions = currentHooks().Sessions()
	}

	for i, rec := range recs {
		v := reconcile.CopyView{Alive: alive[i]}

		switch {
		case alive[i]:
			if mode != observeReadOnly {
				markSeen(spec.AppStateFile, rec)
				watchAdopted(spec, rec)
			}
			if rec.Copy >= 1 {
				v.Ready = currentHooks().ObserveProbe(process.InternalHost, rec.Port)
				if v.Ready && !servedNow(spec.AppStateFile, rec) && !portIsOurs(spec.AppStateFile, rec) {
					v.Ready, v.PortTaken = false, true
				}
				v.Sessions = sessions[dialOf(rec)]
				v.SessionsKnown = sessions != nil
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
	return proxy.Pool{Active: dials(plan.Active), Warm: dials(plan.Warm), Draining: dials(plan.Draining)}
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
			if rec.CrashCount != 0 || !rec.LastExitAt.IsZero() || rec.LastExitFailed || rec.LastExitReason != "" {
				rec.CrashCount, rec.LastExitAt, rec.LastExitFailed, rec.LastExitReason = 0, time.Time{}, false, ""
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

	if moveTakenPorts(spec, &st, views) {
		dirty = true
	}

	// A copy that is current again (the manifest was put back before the new
	// version took over) has no use for an earlier --now: its mark would make
	// a later, ordinary redeploy cut its visitors.
	for i, v := range views {
		if !v.Alive || !v.App.SkipDrain || !v.App.DrainingSince.IsZero() {
			continue
		}
		if changed, restart := reconcile.Outdated(spec, st.Revision, v.App); len(changed) == 0 && !restart {
			v.App.SkipDrain = false
			views[i] = v
			st.Put(v.App)
			dirty = true
		}
	}

	// --now marks the copies that run a previous version, or already drain,
	// so that they stop instead of draining: at once if they drain, and at the
	// switch, which comes in a later cycle and may follow a restart of the
	// daemon, if they serve. The mark is in the state for that. The new copies
	// are not marked: a later redeploy drains them.
	if o.skipDrain {
		for i, v := range views {
			if !v.Alive || v.App.SkipDrain {
				continue
			}
			changed, restart := reconcile.Outdated(spec, st.Revision, v.App)
			if v.App.DrainingSince.IsZero() && len(changed) == 0 && !restart {
				continue
			}
			v.App.SkipDrain = true
			views[i] = v
			st.Put(v.App)
			dirty = true
		}
	}

	now := time.Now()
	plan := reconcile.DecideCopies(spec, reconcile.Input{Revision: st.Revision, Copies: views, Now: now, SkipDrain: o.skipDrain, Settle: currentHooks().DrainSettle})

	// The copies that start draining are marked in the state before anything
	// is pushed or stopped, so a daemon that restarts from here on knows them.
	for _, n := range plan.Drain {
		if rec, ok := st.Copy(n); ok {
			rec.DrainingSince = now
			st.Put(rec)
			dirty = true
		}
	}
	if dirty {
		if err := state.Save(spec.AppStateFile, st); err != nil {
			return api.ApplyResponse{}, err
		}
	}
	if len(plan.Drain) > 0 {
		why := "fewer copies wanted"
		if plan.Switched {
			why = "the new version is ready"
		}
		log.Printf("reconcile: %q: %s; draining %s for up to %s", spec.AppName, why, reconcile.ListCopies(plan.Drain), reconcile.FormatDuration(spec.EffectiveDrainTimeout()))
	}

	resp := api.ApplyResponse{Action: plan.Action(), Message: reconcile.Describe(spec, views, plan, o.settings)}

	// Who serves, before anything is stopped or started.
	pool := poolOf(plan, st)
	if cur, ok := gateway.Pool(spec.AppStateFile); !ok || !samePool(cur, pool) {
		log.Printf("reconcile: %q: %s", spec.AppName, reconcile.DescribeRoles(plan.Active, plan.Warm, plan.Draining))
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

// execute carries out plan's steps, in order: stop what is to stop (surplus
// copies, drained ones, the process an upgrade replaces), check the port,
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

	if err := stopCopies(spec, st, stops); err != nil {
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
	// refused port leaves the app out of the load balancer's settings. Not
	// being able to start is not a crash: nothing new ran, so none is
	// recorded.
	if err := claimPort(spec); err != nil {
		setNotStarted(spec, err)
		return notStartedError{err}
	}

	for _, step := range starts {
		if err := launchCopy(spec, st, step.Copy); err != nil {
			return err
		}
	}
	clearNotStarted(spec.AppStateFile)
	return nil
}

// stopCopies stops the processes of the stop steps in parallel and drops
// their records: copies that are surplus, drained, timed out or that must not
// drain, and the process of an app from before the load balancer, which holds
// the app's own port. A step without a process only drops the record. Stop
// re-checks identity before each signal, so a PID that changed hands since the
// observation is left alone. A copy that fails to stop keeps its record; the
// others are removed.
func stopCopies(spec manifest.AppSpec, st *state.State, stops []reconcile.Step) error {
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
		logStop(spec, step, rec)
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

// logStop writes the line for one stop of a process, by the reason.
func logStop(spec manifest.AppSpec, step reconcile.Step, rec state.AppState) {
	switch step.Why {
	case reconcile.StopUpgrade:
		log.Printf("reconcile: %q: moving behind the load balancer: stopping the previous instance (pid %d), which holds port %d itself", spec.AppName, rec.PID, rec.Port)
	case reconcile.StopOutdated:
		log.Printf("reconcile: %q: stopping copy %d (pid %d): it runs a previous version and is not ready", spec.AppName, step.Copy, rec.PID)
	case reconcile.StopIdle:
		log.Printf("reconcile: %q: stopping copy %d (pid %d): the new version has taken over and it has no visitors", spec.AppName, step.Copy, rec.PID)
	case reconcile.StopDrained:
		log.Printf("reconcile: %q: stopping copy %d (pid %d): it has drained", spec.AppName, step.Copy, rec.PID)
	case reconcile.StopTimeout:
		msg := fmt.Sprintf("reconcile: %q: stopping copy %d (pid %d): drain-timeout passed", spec.AppName, step.Copy, rec.PID)
		if step.Sessions > 0 {
			msg += fmt.Sprintf(" with %d session(s) open", step.Sessions)
		}
		log.Print(msg)
	case reconcile.StopNoDrain:
		log.Printf("reconcile: %q: stopping copy %d (pid %d) without draining", spec.AppName, step.Copy, rec.PID)
	case reconcile.StopTooMany:
		log.Printf("reconcile: %q: stopping copy %d (pid %d): more copies with visitors are draining than the app runs", spec.AppName, step.Copy, rec.PID)
	default:
		log.Printf("reconcile: %q: stopping copy %d (pid %d): fewer copies wanted", spec.AppName, step.Copy, rec.PID)
	}
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
		newApp.LastExitReason = old.LastExitReason
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
