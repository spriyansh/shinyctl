/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/state"
)

// Another user's process takes the internal port a redeploy picked for the new
// copy while that copy's R is still starting: two shinyctl-api daemons on one
// machine can pick the same port. The stranger accepts connections, but it is
// not the copy. The new copy is not ready, no visitor reaches the stranger,
// the old copies keep serving, and the new copy is moved to another port and
// takes over there. Nothing is counted as a crash.
func TestRedeploy_ForeignListenerOnTheNewCopysPort(t *testing.T) {
	useFakeRscript(t)
	settleNow(t)
	buf := captureLog(t)
	spec := slowSpec(t, 1, 0, "1500ms")
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	old := copyByDial(t, spec, pool.Active[0])

	restartResponse(t, spec)
	fresh := newCopies(t, spec, old)[0]
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", fresh.Port))
	if err != nil {
		t.Fatalf("could not take the port first: %v", err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "pid=999999") }))

	// The copy is stopped at once, not left to crash on the bind when its R
	// gets to it (that is 1.5 s away here).
	waitUntil(t, "the copy to be stopped", time.Second, func() bool { return !process.IsAlive(spec, fresh) })

	// For as long as the redeploy lasts nobody is sent to the stranger, and the
	// old copy answers until the new one is ready on a port of its own.
	foreignDial := fmt.Sprintf("127.0.0.1:%d", fresh.Port)
	var movedTo int
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if p, _ := gateway.Pool(spec.AppStateFile); slices.Contains(p.Active, foreignDial) || slices.Contains(p.Warm, foreignDial) {
			t.Fatalf("pool = %+v: the stranger's port is in the table", p)
		}
		if pid, _ := visitWith(t, spec, nil, browserAccept); pid == 999999 {
			t.Fatal("a visitor was served by the stranger")
		}
		rec, _ := loadCopyRecord(spec, fresh.Copy)
		if rec.PID != 0 && rec.Port != fresh.Port {
			movedTo = rec.Port
			if p, _ := gateway.Pool(spec.AppStateFile); len(p.Active) == 1 && p.Active[0] == dialOf(rec) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if movedTo == 0 {
		t.Fatalf("the new copy was never moved off port %d: %+v", fresh.Port, loadCopies(t, spec))
	}
	rec, _ := loadCopyRecord(spec, fresh.Copy)
	if p, _ := gateway.Pool(spec.AppStateFile); len(p.Active) != 1 || p.Active[0] != dialOf(rec) {
		t.Errorf("pool = %+v, want the new copy on port %d active", p, rec.Port)
	}
	if rec.CrashCount != 0 || !rec.LastExitAt.IsZero() {
		t.Errorf("the moved copy = %+v, want no crash counted", rec)
	}
	if pid, _ := visit(t, spec, nil); pid != rec.PID {
		t.Errorf("a new visitor is served by pid %d, want the new copy %d", pid, rec.PID)
	}
	log := buf.String()
	if !strings.Contains(log, fmt.Sprintf("copy %d: port %d is held by another program", fresh.Copy, fresh.Port)) {
		t.Errorf("the log does not say the port is held by another program:\n%s", log)
	}
	if n := strings.Count(log, "is held by another program"); n != 1 {
		t.Errorf("the log says it %d times, want once", n)
	}
	waitStopped(t, spec, old)
}

// The owner is asked once per copy and port, not on every cycle: a socket a
// live process holds can't be taken over.
func TestPortIsOurs_IsAskedOncePerCopy(t *testing.T) {
	var asked atomic.Int32
	useHooks(t, hooks{Owner: func(pid, port int) process.Owner { asked.Add(1); return process.OwnerSelf }})
	rec := state.AppState{PID: 910001, Port: 29001}
	for i := 0; i < 5; i++ {
		if !portIsOurs("s", rec) {
			t.Fatal("a copy that holds its port is not ours")
		}
	}
	if asked.Load() != 1 {
		t.Errorf("the owner was asked %d times, want once", asked.Load())
	}
	// Another copy, or another port, is asked again.
	portIsOurs("s", state.AppState{PID: 910001, Port: 29002})
	portIsOurs("s", state.AppState{PID: 910002, Port: 29001})
	if asked.Load() != 3 {
		t.Errorf("the owner was asked %d times for three different copies, want 3", asked.Load())
	}
}

// A foreign owner is never remembered (it must be looked at again), and
// where the owner can't be told the port counts as the copy's.
func TestPortIsOurs_OtherIsNotRememberedAndUnknownFallsBack(t *testing.T) {
	answer := process.OwnerOther
	useHooks(t, hooks{Owner: func(int, int) process.Owner { return answer }})
	rec := state.AppState{PID: 910003, Port: 29003}
	for i := 0; i < 3; i++ {
		if portIsOurs("s", rec) {
			t.Fatalf("look %d: a port held by another program was taken for the copy's", i+1)
		}
	}
	answer = process.OwnerUnknown
	if !portIsOurs("s", rec) {
		t.Error("where the owner can't be told, the port is taken for the copy's")
	}
	answer = process.OwnerNone
	if !portIsOurs("s", state.AppState{PID: 910004, Port: 29004}) {
		t.Error("with nobody found listening (a race), the port is taken for the copy's")
	}
}

// A copy's port is picked by scanning from the start the hook gives, not from
// the bottom of the range: each user's daemon has its own.
func TestPickPortStart_CopyPortScansFromTheHook(t *testing.T) {
	useHooks(t, hooks{PortStart: func() int { return 27500 }})
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { postJSON(t, handleDelete, spec) })
	st := state.State{AppGroup: spec.AppGroup}
	port, err := copyPort(spec, &st, 1)
	if err != nil {
		t.Fatal(err)
	}
	if port < 27500 || port > 27600 {
		t.Errorf("copyPort = %d, want a port from 27500 on", port)
	}
}

// "Can't tell" is remembered for a while, so a lookup that fails or hangs is
// not paid again on every cycle.
func TestPortIsOurs_UnknownIsRememberedForAWhile(t *testing.T) {
	var asked atomic.Int32
	useHooks(t, hooks{Owner: func(int, int) process.Owner { asked.Add(1); return process.OwnerUnknown }})
	rec := state.AppState{PID: 910010, Port: 29010}
	for i := 0; i < 5; i++ {
		if !portIsOurs("s", rec) {
			t.Fatal("where the owner can't be told, the port is the copy's")
		}
	}
	if asked.Load() != 1 {
		t.Errorf("the owner was asked %d times, want once", asked.Load())
	}
	until, _ := unknownPorts.Load(pidPort{rec.PID, rec.Port})
	if d := time.Until(until.(time.Time)); d <= 0 || d > unknownFor {
		t.Errorf("\"can't tell\" is remembered for %v, want up to %v", d, unknownFor)
	}
	unknownPorts.Store(pidPort{rec.PID, rec.Port}, time.Now().Add(-time.Second)) // the memory has run out
	portIsOurs("s", rec)
	if asked.Load() != 2 {
		t.Errorf("after the memory ran out the owner was asked %d times in all, want 2", asked.Load())
	}
}

// A copy whose own port keeps being judged another program's (a process that
// hides its descriptors, say) is moved three times and then trusted, with one
// line in the log, and takes over.
func TestRedeploy_AFalseForeignJudgementIsBrakedAfterThreeMoves(t *testing.T) {
	useFakeRscript(t)
	settleNow(t)
	buf := captureLog(t)
	var armed atomic.Bool
	useHooks(t, hooks{DrainSettle: time.Nanosecond, Owner: func(pid, port int) process.Owner {
		if !armed.Load() {
			return process.OwnerSelf
		}
		return process.OwnerOther // the new copy's own port is judged foreign, always
	}})
	spec := slowSpec(t, 1, 0, "300ms")
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	old := copyByDial(t, spec, pool.Active[0])
	armed.Store(true)
	restartResponse(t, spec)
	waitPool(t, spec, "the new copy to take over after the brake", func(p proxy.Pool) bool {
		return len(p.Active) == 1 && p.Active[0] != pool.Active[0]
	})
	log := buf.String()
	if n := strings.Count(log, "is held by another program"); n != maxMoves {
		t.Errorf("the copy was moved %d times, want %d:\n%s", n, maxMoves, log)
	}
	if n := strings.Count(log, "is trusted on port"); n != 1 {
		t.Errorf("the log says the copy is trusted %d times, want once:\n%s", n, log)
	}
	if rec := newestApp(t, spec); rec.CrashCount != 0 {
		t.Errorf("the copy = %+v, want no crash counted", rec)
	}
	waitStopped(t, spec, old)
}

// A copy the load balancer already serves keeps its port whatever a lookup
// says later: it is never stopped and moved for a judgement made about it
// afterwards.
func TestPortIsOurs_AServingCopyIsNeverMoved(t *testing.T) {
	useFakeRscript(t)
	var armed atomic.Bool
	useHooks(t, hooks{Owner: func(int, int) process.Owner {
		if armed.Load() {
			return process.OwnerOther
		}
		return process.OwnerSelf
	}})
	spec := copiesSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	old := copyByDial(t, spec, pool.Active[0])
	armed.Store(true)
	ownedPorts.Delete(pidPort{old.PID, old.Port}) // the lookup is made again
	for i := 0; i < 3; i++ {
		reconcileApp(spec.AppStateFile)
	}
	if got, _ := gateway.Pool(spec.AppStateFile); len(got.Active) != 1 || got.Active[0] != pool.Active[0] || !process.IsAlive(spec, old) {
		t.Errorf("pool %+v, alive %v: a serving copy was moved or dropped", got, process.IsAlive(spec, old))
	}
}

// The count of moves in a row starts again once the owner is seen to be the
// copy's own.
func TestPortIsOurs_AnOwnedPortStartsTheCountAgain(t *testing.T) {
	useHooks(t, hooks{Owner: func(int, int) process.Owner { return process.OwnerSelf }})
	ck := seenKey{"s-count", 3}
	portMoves.Store(ck, 2)
	if !portIsOurs("s-count", state.AppState{PID: 910020, Port: 29020, Copy: 3}) {
		t.Fatal("a copy that holds its port is ours")
	}
	if _, ok := portMoves.Load(ck); ok {
		t.Error("the count of moves was kept after the owner was seen to be the copy's")
	}
}

// The brake covers the process it was given for. A copy that crashes and
// starts again is a new process: it is judged afresh, and a stranger that
// takes its port during the start-up gets no visitors.
func TestRedeploy_TheBrakeCoversOnlyTheProcessItWasGivenFor(t *testing.T) {
	useFakeRscript(t)
	settleNow(t)
	var armed, real atomic.Bool
	useHooks(t, hooks{DrainSettle: time.Nanosecond, Owner: func(pid, port int) process.Owner {
		if real.Load() {
			return process.PortOwner(pid, port)
		}
		if !armed.Load() {
			return process.OwnerSelf
		}
		return process.OwnerOther // a wrong judgement of the copy's own port
	}})
	spec := slowSpec(t, 1, 0, "1500ms")
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	armed.Store(true)
	restartResponse(t, spec)
	waitPool(t, spec, "the braked copy to take over", func(p proxy.Pool) bool {
		return len(p.Active) == 1 && p.Active[0] != pool.Active[0]
	})
	real.Store(true) // from now on the owner check tells the truth
	braked := newestApp(t, spec)
	if _, ok := brakes.Load(pidPort{braked.PID, braked.Port}); !ok {
		t.Fatalf("copy %d (pid %d, port %d) was not braked", braked.Copy, braked.PID, braked.Port)
	}
	if n, _ := portMoves.Load(seenKey{spec.AppStateFile, braked.Copy}); n != nil {
		t.Errorf("the count of moves is %v after the brake, want it cleared", n)
	}

	syscall.Kill(braked.PID, syscall.SIGKILL) // it crashes
	var again state.AppState
	waitUntil(t, "the copy to start again", 20*time.Second, func() bool {
		reconcileApp(spec.AppStateFile)
		r, ok := loadCopyRecord(spec, braked.Copy)
		again = r
		return ok && r.PID != 0 && r.PID != braked.PID
	})
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", again.Port))
	if err != nil {
		t.Fatalf("a stranger could not take port %d: %v", again.Port, err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "pid=999999") }))
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		reconcileApp(spec.AppStateFile)
		if p, _ := gateway.Pool(spec.AppStateFile); slices.Contains(p.Active, fmt.Sprintf("127.0.0.1:%d", again.Port)) {
			t.Fatalf("the stranger's port is in the table: %+v", p)
		}
		if pid, _ := visitWith(t, spec, nil, browserAccept); pid == 999999 {
			t.Fatal("a visitor was served by the stranger after the brake")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rec, _ := loadCopyRecord(spec, braked.Copy); rec.Port == again.Port && rec.PID == again.PID {
		t.Errorf("the restarted copy %+v was left on the stranger's port", rec)
	}
}

// A shinyctl-api that starts next to a load balancer already serving copies
// counts them as served from the start: a lookup that says "another program"
// about one of them cannot stop it and move it away from its visitors.
func TestAdoption_CopiesTheBalancerListsAreServedFromTheStart(t *testing.T) {
	useFakeRscript(t)
	var other atomic.Bool
	useHooks(t, hooks{Owner: func(int, int) process.Owner {
		if other.Load() {
			return process.OwnerOther
		}
		return process.OwnerSelf
	}})
	spec := copiesSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	old := copyByDial(t, spec, pool.Active[0])

	// A new daemon: it knows no pool and no owner; the load balancer holds
	// the table the old daemon sent.
	table, err := testAdmin.Copies()
	if err != nil || len(table[spec.AppStateFile].Active) != 1 {
		t.Fatalf("the load balancer's table = %v, %v", table, err)
	}
	gateway.ReplacePools(proxy.Table{}, gateway.PoolSeq())
	if err := testAdmin.PutCopies(table); err != nil {
		t.Fatal(err)
	}
	ownedPorts.Delete(pidPort{old.PID, old.Port})
	other.Store(true) // the lookup now misjudges the adopted copy

	watchRestoredCopies()
	refreshPools()
	for i := 0; i < 3; i++ {
		reconcileApp(spec.AppStateFile)
	}
	if rec, _ := loadCopyRecord(spec, old.Copy); rec.PID != old.PID || !process.IsAlive(spec, rec) {
		t.Errorf("the adopted copy = %+v, was pid %d: it was stopped and moved", rec, old.PID)
	}
	if p, _ := gateway.Pool(spec.AppStateFile); len(p.Active) != 1 || p.Active[0] != pool.Active[0] {
		t.Errorf("pool = %+v, want the adopted copy still serving", p)
	}
}
