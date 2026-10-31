/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

// This file holds what the cycle does about ports: why an app could not start
// (its port is taken), the check that a copy's port belongs to the copy, the
// brake on that check, and moving a copy off a port another program took.

import (
	"log"
	"slices"
	"sync"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

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

// ownedPorts remembers the (pid, port) pairs whose listening socket has been
// seen to belong to the process: a socket that a live process holds can't be
// taken over, so the lookup is made once per copy, not on every cycle.
var ownedPorts sync.Map

type pidPort struct{ pid, port int }

// unknownPorts remembers, for a while, that the owner of a (pid, port) could
// not be told (the lookup failed, timed out or isn't there), so a failing or
// hanging lookup isn't paid again on every cycle, under the state lock.
var unknownPorts sync.Map

// unknownFor is how long "can't tell" is remembered.
const unknownFor = 30 * time.Second

// portMoves counts, per copy, the moves made in a row because its port was
// judged to be another program's, and brakes remembers the processes (pid and
// port) that were given up on after maxMoves: from then on that process's port
// is taken for its own when it accepts connections, as it was before the owner
// could be asked. It covers that process only: a copy that crashes and starts
// again is a new process, judged afresh, and its count starts again.
var (
	portMoves sync.Map // seenKey -> int
	brakes    sync.Map // pidPort -> struct{}
)

const maxMoves = 3

// portIsOurs reports whether the socket listening on rec's port is not
// another program's. A copy counts as ready, and so gets a role and visitors,
// only when it is: another user's shinyctl-api can pick the same internal
// port, and its R, or any program, can win the bind. Where the owner can't be
// told (another system, a failed lookup), or the copy has been moved
// maxMoves times in a row, a port that accepts connections is taken to be the
// copy's, as it always was.
func portIsOurs(stateFile string, rec state.AppState) bool {
	key := pidPort{rec.PID, rec.Port}
	if _, ok := ownedPorts.Load(key); ok {
		return true
	}
	if until, ok := unknownPorts.Load(key); ok && time.Now().Before(until.(time.Time)) {
		return true
	}
	ck := seenKey{stateFile, rec.Copy}
	if _, ok := brakes.Load(key); ok {
		return true
	}
	switch currentHooks().Owner(rec.PID, rec.Port) {
	case process.OwnerOther:
		return false
	case process.OwnerSelf:
		ownedPorts.Store(key, struct{}{})
		portMoves.Delete(ck)
	case process.OwnerUnknown:
		unknownPorts.Store(key, time.Now().Add(unknownFor))
	}
	return true
}

// allDials lists every copy of a pool.
func allDials(p proxy.Pool) []string { return slices.Concat(p.Active, p.Warm, p.Draining) }

// servedNow says the load balancer already sends visitors to the copy: its
// port has been the copy's all along, whatever a lookup says now.
func servedNow(stateFile string, rec state.AppState) bool {
	pool, _ := gateway.Pool(stateFile)
	dial := dialOf(rec)
	return slices.Contains(pool.Active, dial) || slices.Contains(pool.Warm, dial) || slices.Contains(pool.Draining, dial)
}

// moveTakenPorts gives each copy whose port another program holds a new one,
// at once, instead of waiting for it to crash on the bind: the process is
// stopped, its record keeps the crash history but not the process, and the
// decision that follows starts it again, which finds the recorded port busy
// and moves it to a free one. Nothing is counted as a crash: the copy did
// nothing wrong. It never goes into the table meanwhile. A copy that is
// judged foreign maxMoves times in a row is not moved again (the judgement is
// wrong for it: a process that hides its descriptors, say): it is left to
// the TCP check, once, with a line in the log.
func moveTakenPorts(spec manifest.AppSpec, st *state.State, views []reconcile.CopyView) (changed bool) {
	for i, v := range views {
		if !v.PortTaken || !v.Alive || !v.App.DrainingSince.IsZero() {
			continue
		}
		rec := v.App
		ck := seenKey{spec.AppStateFile, rec.Copy}
		n := 0
		if c, ok := portMoves.Load(ck); ok {
			n = c.(int)
		}
		if n >= maxMoves {
			brakes.Store(pidPort{rec.PID, rec.Port}, struct{}{})
			portMoves.Delete(ck)
			log.Printf("reconcile: %q: copy %d has been moved %d times because its port looked like another program's; this process is trusted on port %d", spec.AppName, rec.Copy, maxMoves, rec.Port)
			views[i].Ready, views[i].PortTaken = true, false
			continue
		}
		log.Printf("reconcile: %q: copy %d: port %d is held by another program; stopping the copy to start it again on another port", spec.AppName, rec.Copy, rec.Port)
		if _, err := process.Stop(rec, identityFallbacks(spec)...); err != nil {
			log.Printf("reconcile: %q: copy %d: stopping it: %v", spec.AppName, rec.Copy, err)
			continue
		}
		portMoves.Store(ck, n+1)
		forgetSeen(spec.AppStateFile, rec.Copy)
		moved := state.AppState{Copy: rec.Copy, Port: rec.Port, CrashCount: rec.CrashCount, LastExitAt: rec.LastExitAt, LastExitFailed: rec.LastExitFailed, LastExitReason: rec.LastExitReason}
		st.Put(moved)
		views[i] = reconcile.CopyView{App: moved, SessionsKnown: v.SessionsKnown}
		changed = true
	}
	return changed
}
