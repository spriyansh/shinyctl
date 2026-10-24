/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// copiesSpec is an app with replicas copies that serve and warm spares.
func copiesSpec(t *testing.T, name string, replicas, warm int) manifest.AppSpec {
	t.Helper()
	spec := newTestSpec(t, t.TempDir(), name, "app.R", freePorts(t, 1)[0])
	spec.Replicas, spec.WarmReplicas = replicas, &warm
	return spec
}

// cleanupApp deletes the app when the test ends, and kills whatever of its
// copies is left.
func cleanupApp(t *testing.T, spec manifest.AppSpec) {
	t.Helper()
	t.Cleanup(func() {
		recs, _ := state.Load(spec.AppStateFile)
		postJSON(t, handleDelete, spec)
		for _, rec := range recs.Copies() {
			killIfAlive(spec, rec)
		}
	})
}

func applyResponse(t *testing.T, spec manifest.AppSpec) api.ApplyResponse {
	t.Helper()
	rec := postJSON(t, handleApply, spec)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply %q: status = %d, body = %s", spec.AppName, rec.Code, rec.Body.String())
	}
	return decodeApplyResponse(t, rec)
}

// copyByDial finds the record whose process listens at dial.
func copyByDial(t *testing.T, spec manifest.AppSpec, dial string) state.AppState {
	t.Helper()
	for _, rec := range loadCopies(t, spec) {
		if dialOf(rec) == dial {
			return rec
		}
	}
	t.Fatalf("no copy of %q listens at %s: %+v", spec.AppName, dial, loadCopies(t, spec))
	return state.AppState{}
}

// waitRoles waits until the load balancer's pool for the app has the given
// number of active and warm copies, and returns it.
func waitRoles(t *testing.T, spec manifest.AppSpec, active, warm int) proxy.Pool {
	t.Helper()
	var pool proxy.Pool
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pool, _ = gateway.Pool(spec.AppStateFile)
		if len(pool.Active) == active && len(pool.Warm) == warm {
			return pool
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the pool is %+v after 10s, want %d active and %d warm", pool, active, warm)
	return pool
}

// visit makes one request through the app's public address on a new
// connection, with the cookie if given. It returns the pid that answered and
// the cookie the load balancer set, if it set one.
func visit(t *testing.T, spec manifest.AppSpec, cookie *http.Cookie) (pid int, set *http.Cookie) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+dialAddr(spec.EffectiveHost(), spec.Port)+"/", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", req.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", req.URL, resp.StatusCode, body)
	}
	pid, err = strconv.Atoi(strings.TrimPrefix(string(body), "pid="))
	if err != nil {
		t.Fatalf("GET %s: body %q", req.URL, body)
	}
	for _, c := range resp.Cookies() {
		set = c
	}
	return pid, set
}

// holdOpen starts a request that stays open until the test ends (a Shiny
// session, as far as the load balancer can tell) and returns once the load
// balancer has answered its headers.
func holdOpen(t *testing.T, spec manifest.AppSpec) {
	t.Helper()
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Get("http://" + dialAddr(spec.EffectiveHost(), spec.Port) + "/hold")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
}

// tick is what the ticker would do, for tests: reconcile the app every so
// often until stop is called. The daemon's tick is every five seconds.
func tick(spec manifest.AppSpec) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
				if reconcileApp(spec.AppStateFile) {
					syncProxy("")
				}
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s: not true after %s", what, timeout)
}

func TestApply_StartsCopiesAndWarm(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 2, 1)
	cleanupApp(t, spec)

	resp := applyResponse(t, spec)
	if want := fmt.Sprintf("Starting app %q on port %d (2 copies and 1 warm spare)", spec.AppName, spec.Port); resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}

	recs := loadCopies(t, spec)
	ports := map[int]bool{}
	for _, rec := range recs {
		ports[rec.Port] = true
		if rec.Port < manifest.InternalPortMin || rec.Port > manifest.InternalPortMax {
			t.Errorf("copy %d is on port %d, want an internal one", rec.Copy, rec.Port)
		}
	}
	if len(recs) != 3 || len(ports) != 3 {
		t.Fatalf("records = %+v, want three copies on three distinct ports", recs)
	}

	pool := waitRoles(t, spec, 2, 1)
	byStart := slices.Clone(recs)
	slices.SortFunc(byStart, func(a, b state.AppState) int { return a.StartedAt.Compare(b.StartedAt) })
	var wantActive []string
	for _, rec := range byStart[:2] {
		wantActive = append(wantActive, dialOf(rec))
	}
	if !slices.Equal(pool.Active, wantActive) || pool.Warm[0] != dialOf(byStart[2]) {
		t.Errorf("pool = %+v, want the two oldest active (%v) and the newest warm", pool, wantActive)
	}

	reached := map[int]int{}
	for i := 0; i < 30; i++ {
		pid, _ := visit(t, spec, nil)
		reached[pid]++
	}
	if len(reached) != 2 || reached[byStart[2].PID] != 0 {
		t.Errorf("30 new visitors reached %v, want only the two active copies %d and %d", reached, byStart[0].PID, byStart[1].PID)
	}
}

func TestTakeover_ActiveDiesWarmServesAtOnce(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active, warm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	pid, cookie := visit(t, spec, nil)
	if pid != active.PID || cookie == nil || cookie.Name != proxy.CookieName(spec.Port) {
		t.Fatalf("a new visitor reached pid %d with cookie %v, want the active copy %d and a %s cookie", pid, cookie, active.PID, proxy.CookieName(spec.Port))
	}
	reloads := gateway.Reloads()

	if err := syscall.Kill(active.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the warm copy to be promoted", 500*time.Millisecond, func() bool {
		p, _ := gateway.Pool(spec.AppStateFile)
		return slices.Equal(p.Active, []string{dialOf(warm)})
	})
	if pid, set := visit(t, spec, cookie); pid != warm.PID || set == nil {
		t.Errorf("the visitor of the dead copy reached pid %d with cookie %v, want the warm copy %d with a new cookie", pid, set, warm.PID)
	}
	if got := gateway.Reloads(); got != reloads {
		t.Errorf("the load balancer was reloaded %d time(s) for a takeover", got-reloads)
	}

	// The killed copy comes back, newest, so it is the spare: roles don't swap
	// back on their own.
	stop := tick(spec)
	defer stop()
	waitUntil(t, "the killed copy to be back as the warm copy", 12*time.Second, func() bool {
		p, _ := gateway.Pool(spec.AppStateFile)
		back, ok := loadCopyRecord(spec, active.Copy)
		return ok && back.PID != active.PID && process.IsAlive(spec, back) && slices.Equal(p.Warm, []string{dialOf(back)}) && slices.Equal(p.Active, []string{dialOf(warm)})
	})
	back, _ := loadCopyRecord(spec, active.Copy)
	other, _ := loadCopyRecord(spec, warm.Copy)
	if back.CrashCount != 1 || other.CrashCount != 0 || other.PID != warm.PID {
		t.Errorf("crash counts: killed copy %d, other copy %d (pid %d -> %d), want 1 and 0 with the other untouched", back.CrashCount, other.CrashCount, warm.PID, other.PID)
	}
}

func loadCopyRecord(spec manifest.AppSpec, n int) (state.AppState, bool) {
	st, err := state.Load(spec.AppStateFile)
	if err != nil {
		return state.AppState{}, false
	}
	return st.Copy(n)
}

// TestTakeover_TickBeforeWatcher: a tick that finds the active copy gone
// before its watcher recorded the exit starts nothing, but still gives the
// visitors to the warm copy.
func TestTakeover_TickBeforeWatcher(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active, warm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	unlock := state.Lock(spec.AppStateFile)
	if err := syscall.Kill(active.PID, syscall.SIGKILL); err != nil {
		unlock()
		t.Fatal(err)
	}
	waitGone(t, active)

	resp, err := reconcileLocked(spec, reconcileOpts{})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Action != reconcile.ActionNone {
		t.Errorf("the tick answered %q while the exit was pending, want none", resp.Action)
	}
	if p, _ := gateway.Pool(spec.AppStateFile); !slices.Equal(p.Active, []string{dialOf(warm)}) {
		t.Errorf("pool = %+v, want the warm copy active", p)
	}
	if rec, _ := loadCopyRecord(spec, active.Copy); rec.PID != active.PID && rec.CrashCount == 0 && rec.PID != 0 {
		t.Errorf("the tick started copy %d (pid %d -> %d) although its exit was pending", active.Copy, active.PID, rec.PID)
	}

	// The watcher records the exit once the lock is free, and the copy comes
	// back after its backoff.
	stop := tick(spec)
	defer stop()
	waitUntil(t, "the copy to be started by the watcher's reconcile", 12*time.Second, func() bool {
		rec, _ := loadCopyRecord(spec, active.Copy)
		return rec.PID != active.PID && rec.PID > 0 && rec.CrashCount == 1
	})
}

// TestTakeover_PushComesBeforeTheRestart: the warm copy is given the visitors
// before the dead one's start, which can wait for a port, is attempted.
func TestTakeover_PushComesBeforeTheRestart(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active, warm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	useHooks(t, hooks{StartDelay: 2 * time.Second})

	// The state lock is held, so the dead copy's exit-watcher can't reconcile
	// first and make the push itself: only the cycle below does.
	unlock := state.Lock(spec.AppStateFile)
	defer unlock()
	if err := syscall.Kill(active.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitGone(t, active)

	// "Run it now": the dead copy is started at once, after the delay, and the
	// pool is pushed before that.
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconcileLocked(spec, reconcileOpts{explicit: true})
	}()
	// What the load balancer itself holds, not what the gateway believes.
	waitUntil(t, "the load balancer to list the warm copy as active", 300*time.Millisecond, func() bool {
		table, err := testAdmin.Copies()
		return err == nil && slices.Equal(table[spec.AppStateFile].Active, []string{dialOf(warm)})
	})
	select {
	case <-done:
		t.Fatal("the cycle finished before the start delay: nothing was delayed")
	default:
	}
	<-done
}

func TestNotStarted_LoggedOnce(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 0)
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { postJSON(t, handleDelete, spec) })

	ln, err := net.Listen("tcp4", net.JoinHostPort(spec.Host, strconv.Itoa(spec.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var logs logBuffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	for i := 0; i < 3; i++ {
		if reconcileApp(spec.AppStateFile) {
			t.Fatal("an app whose port is taken was started")
		}
	}
	if got := strings.Count(logs.String(), "not started:"); got != 1 {
		t.Errorf("the log has %d 'not started:' lines after three reconciles, want 1:\n%s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "is not available"); got != 1 {
		t.Errorf("the log says the port is not available %d times, want 1:\n%s", got, logs.String())
	}
	if rec := loadApp(t, spec); rec.CrashCount != 0 || !rec.LastExitAt.IsZero() {
		t.Errorf("recorded app = %+v, want no exit recorded", rec)
	}
	if reason := notStartedReason(spec.AppStateFile); !strings.Contains(reason, "is not available") {
		t.Errorf("notStartedReason = %q", reason)
	}

	// Once the port is free the app starts and the reason goes away.
	ln.Close()
	if !reconcileApp(spec.AppStateFile) {
		t.Fatal("the app did not start once its port was free")
	}
	if reason := notStartedReason(spec.AppStateFile); reason != "" {
		t.Errorf("notStartedReason = %q after the start, want none", reason)
	}
	if rec := loadApp(t, spec); rec.PID > 0 {
		killIfAlive(spec, rec)
	}
}

func TestScaleUp_LeavesRunningCopiesAlone(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	waitRoles(t, spec, 1, 1)
	before := loadCopies(t, spec)
	reloads := gateway.Reloads()

	bigger := spec
	bigger.Replicas = 2
	resp := applyResponse(t, bigger)
	if want := fmt.Sprintf("Updated %q without a restart (manifest changed: replicas; starting copy 3)", spec.AppName); resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}

	after := loadCopies(t, spec)
	if len(after) != 3 {
		t.Fatalf("records = %+v, want three copies", after)
	}
	for i, rec := range before {
		if after[i].PID != rec.PID || after[i].Port != rec.Port {
			t.Errorf("copy %d changed: pid %d -> %d", rec.Copy, rec.PID, after[i].PID)
		}
	}

	waitRoles(t, bigger, 2, 1)
	if got := gateway.Reloads(); got != reloads {
		t.Errorf("the load balancer was reloaded %d time(s) for a scale-up", got-reloads)
	}
	// Copy 2, the spare, serves now.
	pool, _ := gateway.Pool(spec.AppStateFile)
	if !slices.Contains(pool.Active, dialOf(before[1])) {
		t.Errorf("pool = %+v, want the old spare promoted to active", pool)
	}
}

func TestScaleDown_StopsWarmThenFewestSessions(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 3, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 3, 1)
	warmRec := copyByDial(t, spec, pool.Warm[0])
	reloads := gateway.Reloads()

	// Sessions on two of the three active copies.
	holdOpen(t, spec)
	holdOpen(t, spec)
	var held, free []state.AppState
	waitUntil(t, "two sessions to be counted", 5*time.Second, func() bool {
		sessions := gateway.Sessions()
		held, free = nil, nil
		for _, dial := range pool.Active {
			if sessions[dial] == 1 {
				held = append(held, copyByDial(t, spec, dial))
			} else {
				free = append(free, copyByDial(t, spec, dial))
			}
		}
		return len(held) == 2 && len(free) == 1
	})

	smaller := spec
	smaller.Replicas = 1
	resp := applyResponse(t, smaller)
	if !strings.Contains(resp.Message, "without a restart (manifest changed: replicas; stopping copies ") {
		t.Errorf("message = %q", resp.Message)
	}

	for _, rec := range []state.AppState{warmRec, free[0]} {
		if process.IsAlive(spec, rec) {
			t.Errorf("copy %d (pid %d) is still running, want it stopped", rec.Copy, rec.PID)
		}
	}
	for _, rec := range held {
		if !process.IsAlive(spec, rec) {
			t.Errorf("copy %d (pid %d) holds a session and was stopped", rec.Copy, rec.PID)
		}
	}

	oldest, newest := held[0], held[1]
	if newest.StartedAt.Before(oldest.StartedAt) {
		oldest, newest = newest, oldest
	}
	got := waitRoles(t, smaller, 1, 1)
	if got.Active[0] != dialOf(oldest) || got.Warm[0] != dialOf(newest) {
		t.Errorf("pool = %+v, want the older remaining copy active and the other warm", got)
	}
	if n := gateway.Reloads(); n != reloads {
		t.Errorf("the load balancer was reloaded %d time(s) for a scale-down", n-reloads)
	}
	if recs := loadCopies(t, spec); len(recs) != 2 {
		t.Errorf("records = %+v, want two copies", recs)
	}
}

func TestScaleDown_ToOneCopy(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active, warm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	none := spec
	zero := 0
	none.WarmReplicas = &zero
	resp := applyResponse(t, none)
	if want := fmt.Sprintf("Updated %q without a restart (manifest changed: warm-replicas; stopping copy %d)", spec.AppName, warm.Copy); resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}
	if process.IsAlive(spec, warm) {
		t.Error("the warm copy is still running")
	}
	if rec, _ := loadCopyRecord(spec, active.Copy); rec.PID != active.PID || !process.IsAlive(spec, rec) {
		t.Errorf("the active copy changed: %+v, want pid %d", rec, active.PID)
	}
	waitRoles(t, none, 1, 0)
	if pid, _ := visit(t, spec, nil); pid != active.PID {
		t.Errorf("served by pid %d, want %d", pid, active.PID)
	}
}

func TestRestart_OneCopyAtATime(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active, warm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	// Visitors all the while.
	var failures []string
	stopVisitors := make(chan struct{})
	visitorsDone := make(chan struct{})
	go func() {
		defer close(visitorsDone)
		client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		for {
			select {
			case <-stopVisitors:
				return
			case <-time.After(20 * time.Millisecond):
			}
			resp, err := client.Get("http://" + dialAddr(spec.EffectiveHost(), spec.Port) + "/")
			if err != nil {
				failures = append(failures, err.Error())
				continue
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				failures = append(failures, fmt.Sprintf("status %d", resp.StatusCode))
			}
		}
	}()

	// Which copy changes first.
	var order []int
	changed := make(chan struct{})
	go func() {
		defer close(changed)
		seen := map[int]bool{}
		for len(seen) < 2 {
			time.Sleep(10 * time.Millisecond)
			for _, orig := range []state.AppState{active, warm} {
				if rec, ok := loadCopyRecord(spec, orig.Copy); ok && rec.PID != orig.PID && rec.PID > 0 && !seen[orig.Copy] {
					seen[orig.Copy] = true
					order = append(order, orig.Copy)
				}
			}
		}
	}()

	resp := restartResponse(t, spec)
	if want := fmt.Sprintf("Restarting app %q one copy at a time: copy %d first (stopped pid %d)", spec.AppName, warm.Copy, warm.PID); resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}

	select {
	case <-changed:
	case <-time.After(15 * time.Second):
		t.Fatalf("both copies were not replaced within 15s: %+v", loadCopies(t, spec))
	}
	waitRoles(t, spec, 1, 1)
	close(stopVisitors)
	<-visitorsDone

	if len(failures) > 0 {
		t.Errorf("%d request(s) failed during the restart: %v", len(failures), failures)
	}
	if !slices.Equal(order, []int{warm.Copy, active.Copy}) {
		t.Errorf("copies were replaced in the order %v, want the warm copy %d first", order, warm.Copy)
	}
	st, _ := state.Load(spec.AppStateFile)
	if st.Revision != 1 {
		t.Errorf("State.Revision = %d, want 1", st.Revision)
	}
	for _, rec := range st.Copies() {
		if rec.Revision != 1 {
			t.Errorf("copy %d has revision %d, want 1", rec.Copy, rec.Revision)
		}
	}
}

// TestRestart_OneCopyAppIsPhase1: with one copy and no spare a restart is
// what it always was, with the same message and the same internal port.
func TestRestart_OneCopyAppIsPhase1(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	a := applyOK(t, spec)
	waitServed(t, spec, a.PID)

	resp := restartResponse(t, spec)
	if want := fmt.Sprintf("Starting app %q on port %d (stopped the previous instance: pid %d)", spec.AppName, spec.Port, a.PID); resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}
	b := loadApp(t, spec)
	if b.PID == a.PID || b.Port != a.Port || b.Revision != 1 {
		t.Errorf("recorded app = %+v, want a new process on port %d at revision 1", b, a.Port)
	}
	waitServed(t, spec, b.PID)
}

// TestApply_EnvChangeRollsAndStallsOnBrokenCopy: a new version that dies at
// start stops the rollout at the first copy; the old copy keeps serving.
func TestApply_EnvChangeRollsAndStallsOnBrokenCopy(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active, warm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	broken := spec
	broken.Env = map[string]string{"FAKE_RSCRIPT_EXIT": "1"}
	resp := applyResponse(t, broken)
	if want := fmt.Sprintf("Restarting app %q one copy at a time (manifest changed: env): copy %d first (stopped pid %d)", spec.AppName, warm.Copy, warm.PID); resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}

	stop := tick(broken)
	defer stop()
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if pid, _ := visit(t, spec, nil); pid != active.PID {
			t.Fatalf("served by pid %d, want the old active copy %d", pid, active.PID)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rec, _ := loadCopyRecord(spec, active.Copy); rec.PID != active.PID || !process.IsAlive(spec, rec) {
		t.Errorf("the old copy was replaced although the new version is broken: %+v", rec)
	}
	if rec, _ := loadCopyRecord(spec, warm.Copy); rec.CrashCount == 0 {
		t.Errorf("the replaced copy has no crash recorded: %+v", rec)
	}

	rec := httptestStatus(t)
	var out bytes.Buffer
	if err := writeStatus(&out, rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "app: restarting one copy at a time; copy 1 still runs the previous version") {
		t.Errorf("status output lacks the rollout line:\n%s", out.String())
	}
	var found bool
	for _, a := range rec.Apps {
		if a.Name != "app" {
			continue
		}
		for _, c := range a.Copies {
			if c.Copy == active.Copy && c.Outdated {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("status does not mark copy %d outdated: %+v", active.Copy, rec.Apps)
	}
}

func httptestStatus(t *testing.T) api.StatusResponse {
	t.Helper()
	rec := newStatusRecorder()
	handleStatus(rec, newStatusRequest())
	return decodeStatus(t, rec)
}

func TestNeverPolicy_PerCopy(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	spec.RestartPolicy = manifest.RestartNever
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active, warm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	if err := syscall.Kill(active.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the exit to be recorded", 5*time.Second, func() bool {
		rec, _ := loadCopyRecord(spec, active.Copy)
		return rec.CrashCount == 1
	})
	waitRoles(t, spec, 1, 0)
	time.Sleep(300 * time.Millisecond)
	if rec, _ := loadCopyRecord(spec, active.Copy); rec.PID != active.PID {
		t.Errorf("a restart-policy never copy was started again: pid %d -> %d", active.PID, rec.PID)
	}
	if pid, _ := visit(t, spec, nil); pid != warm.PID {
		t.Errorf("served by pid %d, want the surviving copy %d", pid, warm.PID)
	}
	for _, c := range statusOf(t, "app").Copies {
		if c.GaveUp != (c.Copy == active.Copy) {
			t.Errorf("copy %d: GaveUp = %v; only the copy that died under `never` has given up", c.Copy, c.GaveUp)
		}
	}
	if !process.IsReady(spec.Host, spec.Port) {
		t.Error("the port closed while a copy is still running")
	}

	if err := syscall.Kill(warm.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, spec.Host, spec.Port)
}

func TestStopAndDelete_AllCopies(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 2, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	waitRoles(t, spec, 2, 1)
	recs := loadCopies(t, spec)

	if rec := postJSON(t, handleStop, spec); rec.Code != http.StatusOK {
		t.Fatalf("stop: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, rec := range recs {
		if process.IsAlive(spec, rec) {
			t.Errorf("copy %d (pid %d) is still running after stop", rec.Copy, rec.PID)
		}
	}
	waitClosed(t, spec.Host, spec.Port)
	if _, ok := gateway.Pool(spec.AppStateFile); ok {
		t.Error("the pool of a stopped app was kept")
	}

	applyOK(t, spec)
	waitRoles(t, spec, 2, 1)
	again := loadCopies(t, spec)
	if len(again) != 3 {
		t.Fatalf("records = %+v after apply, want three copies", again)
	}

	if rec := postJSON(t, handleDelete, spec); rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, rec := range again {
		if process.IsAlive(spec, rec) {
			t.Errorf("copy %d (pid %d) is still running after delete", rec.Copy, rec.PID)
		}
	}
	if _, err := os.Stat(spec.AppStateFile); !os.IsNotExist(err) {
		t.Errorf("state file after delete: %v, want it removed", err)
	}
	if _, ok := gateway.Pool(spec.AppStateFile); ok {
		t.Error("the pool of a deleted app was kept")
	}
}

// TestUpgrade_Phase1RecordGainsAWarmCopy: an app recorded by the previous
// version has one copy and a manifest without warm-replicas, which now means
// one spare. The running copy is left alone and a second one starts.
func TestUpgrade_Phase1RecordGainsAWarmCopy(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	spec.WarmReplicas = nil
	cleanupApp(t, spec)
	a := startCopy(t, spec)
	waitServed(t, spec, a.PID)

	if !reconcileApp(spec.AppStateFile) {
		t.Fatal("the reconcile did not start the spare")
	}
	recs := loadCopies(t, spec)
	if len(recs) != 2 || recs[0].PID != a.PID || recs[1].PID == 0 || recs[1].PID == a.PID {
		t.Fatalf("records = %+v, want copy 1 untouched (pid %d) and a new copy 2", recs, a.PID)
	}
	pool := waitRoles(t, spec, 1, 1)
	if pool.Active[0] != dialOf(recs[0]) {
		t.Errorf("pool = %+v, want the running copy to stay active", pool)
	}
	if recs[0].CrashCount != 0 {
		t.Errorf("copy 1 = %+v, want no crash", recs[0])
	}
}

// TestUpgrade_LegacyRecordMovesBehindLoadBalancer with a spare: the process
// from before the load balancer is stopped and both copies start.
func TestUpgrade_LegacyRecordWithASpare(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	a := startOldStyle(t, spec)

	resp := applyResponse(t, spec)
	want := fmt.Sprintf("Starting app %q on port %d (1 copy and 1 warm spare; moved behind the load balancer; stopped the previous instance: pid %d)", spec.AppName, spec.Port, a.PID)
	if resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}
	if process.IsAlive(spec, a) {
		t.Errorf("the previous process (pid %d) is still running", a.PID)
	}
	recs := loadCopies(t, spec)
	if len(recs) != 2 || recs[0].Copy != 1 || recs[1].Copy != 2 {
		t.Fatalf("records = %+v, want copies 1 and 2", recs)
	}
	waitRoles(t, spec, 1, 1)
	pid, _ := visit(t, spec, nil)
	if pid != recs[0].PID {
		t.Errorf("served by pid %d, want copy 1 (%d)", pid, recs[0].PID)
	}
}

func TestRefreshPools_RebuildsRolesFromState(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	want := waitRoles(t, spec, 1, 1)

	gateway.ReplacePools(proxy.Table{}, gateway.PoolSeq())
	if _, ok := gateway.Pool(spec.AppStateFile); ok {
		t.Fatal("ReplacePools did not clear the pool")
	}

	refreshPools()
	got, _ := gateway.Pool(spec.AppStateFile)
	if !slices.Equal(got.Active, want.Active) || !slices.Equal(got.Warm, want.Warm) {
		t.Errorf("pool = %+v after refreshPools, want %+v", got, want)
	}
}

// TestIdentity_ReusedPIDOfAnotherCopyIsNotOurs: a record that names another
// copy's live process is not alive, since the port differs; the copy is
// started and the other is not touched.
func TestIdentity_ReusedPIDOfAnotherCopyIsNotOurs(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	one, two := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	st, _ := state.Load(spec.AppStateFile)
	impostor := one
	impostor.PID = two.PID
	st.Put(impostor)
	if err := state.Save(spec.AppStateFile, st); err != nil {
		t.Fatal(err)
	}
	if got := process.AliveAll(spec, []state.AppState{impostor, two}); got[0] || !got[1] {
		t.Fatalf("AliveAll = %v, want [false true]", got)
	}

	// Killed first: the real copy 1 is gone, so the record is simply wrong.
	syscall.Kill(one.PID, syscall.SIGKILL)
	waitGone(t, one)

	// An explicit reconcile starts copy 1 and leaves copy 2 alone.
	unlock := state.Lock(spec.AppStateFile)
	_, err := reconcileLocked(spec, reconcileOpts{explicit: true})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	rec1, _ := loadCopyRecord(spec, one.Copy)
	rec2, _ := loadCopyRecord(spec, two.Copy)
	if rec1.PID == two.PID || rec1.PID == one.PID || !process.IsAlive(spec, rec1) {
		t.Errorf("copy 1 = %+v, want a new process of its own", rec1)
	}
	if rec2.PID != two.PID || !process.IsAlive(spec, rec2) {
		t.Errorf("copy 2 = %+v, want pid %d untouched", rec2, two.PID)
	}
}

func TestHandleStatus_PerCopy(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	activeDial, warmDial := pool.Active[0], pool.Warm[0]
	holdOpen(t, spec)
	waitUntil(t, "the session to be counted", 5*time.Second, func() bool { return gateway.Sessions()[activeDial] == 1 })

	resp := httptestStatus(t)
	var app api.AppStatus
	for _, a := range resp.Apps {
		if a.Name == "app" {
			app = a
		}
	}
	if len(app.Copies) != 2 || app.Replicas != 1 || app.WarmReplicas != 1 {
		t.Fatalf("status = %+v, want two copies of 1+1", app)
	}
	for _, c := range app.Copies {
		dial := dialOf(copyByDialRecord(t, spec, c.Copy))
		wantRole, wantSessions := "active", 1
		if dial == warmDial {
			wantRole, wantSessions = "warm", 0
		}
		if c.Role != wantRole || !c.Alive || !c.Ready || c.RSSBytes <= 0 || c.Sessions == nil || *c.Sessions != wantSessions || c.Outdated {
			t.Errorf("copy %d = %+v, want role %s, alive, ready, memory, %d session(s)", c.Copy, c, wantRole, wantSessions)
		}
	}
	if resp.BalancerRSS <= 0 {
		t.Errorf("BalancerRSS = %d", resp.BalancerRSS)
	}

	var out bytes.Buffer
	if err := writeStatus(&out, resp); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out.String(), "\n")
	if !regexp.MustCompile(`^NAME\s+COPY\s+ROLE\s+PORT\s+PID\s+STATUS\s+READY\s+MEM\s+SESSIONS\s+CRASHES\s+STARTED$`).MatchString(lines[0]) {
		t.Errorf("header = %q", lines[0])
	}
	if !strings.Contains(out.String(), "Memory: ") || !strings.Contains(out.String(), "in 2 R processes (1 warm), plus ") || !strings.Contains(out.String(), "for the load balancer.") {
		t.Errorf("no memory line in:\n%s", out.String())
	}
}

func copyByDialRecord(t *testing.T, spec manifest.AppSpec, n int) state.AppState {
	t.Helper()
	rec, ok := loadCopyRecord(spec, n)
	if !ok {
		t.Fatalf("no copy %d", n)
	}
	return rec
}

func TestStart_WritesMarker(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	app, done := startRaw(t, spec, 1)
	t.Cleanup(func() { syscall.Kill(app.PID, syscall.SIGKILL); <-done })

	path, err := process.CopyLogPath(spec, 1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "--- shinyctl: starting copy 1 at ") || !strings.Contains(string(data), " ---\n") {
		t.Errorf("log = %q, want it to start with the marker line", data)
	}

	app2, done2 := startRaw(t, spec, 2)
	t.Cleanup(func() { syscall.Kill(app2.PID, syscall.SIGKILL); <-done2 })
	path2, _ := process.CopyLogPath(spec, 2)
	if data, _ := os.ReadFile(path2); !strings.HasPrefix(string(data), "--- shinyctl: starting copy 2 at ") {
		t.Errorf("copy 2's log = %q", data)
	}
}

// A shinyctl-api that starts next to a load balancer it did not start (a new
// daemon adopting one) tells it who serves what: the supervisor's OnReady, as
// newGateway wires it, rebuilds the table from the state files.
func TestStartup_OnReadyGivesTheAdoptedBalancerItsCopies(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	want := waitRoles(t, spec, 1, 1)

	// The load balancer comes up knowing nothing. The app's state lock is
	// held from here to the end of the check, so that no cycle of its own (a
	// ready-watcher's, say, still in flight) pushes the pool again meanwhile.
	unlock := state.Lock(spec.AppStateFile)
	defer unlock()
	gateway.ReplacePools(proxy.Table{}, gateway.PoolSeq())
	if table, _ := testAdmin.Copies(); len(table[spec.AppStateFile].Active) != 0 {
		t.Fatalf("the load balancer still has %v", table)
	}

	dir, err := os.MkdirTemp("", "sp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	exe, _ := os.Executable()
	_, sup, err := newGateway(dir, exe, filepath.Join(dir, "log"))
	if err != nil {
		t.Fatal(err)
	}
	sup.OnReady()

	table, err := testAdmin.Copies()
	if err != nil || !slices.Equal(table[spec.AppStateFile].Active, want.Active) || !slices.Equal(table[spec.AppStateFile].Warm, want.Warm) {
		t.Errorf("after OnReady the load balancer has %v, %v; want %+v", table[spec.AppStateFile], err, want)
	}
}

// The rollout waits until a replaced copy accepts connections. A new version
// that is slow to start never leaves the app with a copy that is not
// listening in front of its visitors.
func TestRestart_SlowCopyIsWaitedFor(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active, warm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	slow := spec
	slow.Env = map[string]string{"FAKE_RSCRIPT_DELAY": "1500ms"}
	if resp := applyResponse(t, slow); !strings.Contains(resp.Message, "one copy at a time") {
		t.Fatalf("message = %q", resp.Message)
	}

	// While the replaced copy has not started listening, the old active copy
	// is left alone and no request waits.
	var worst time.Duration
	stopTicks := tick(spec) // the ticker's cycles, which see the copy starting
	defer stopTicks()
	for end := time.Now().Add(1200 * time.Millisecond); time.Now().Before(end); time.Sleep(40 * time.Millisecond) {
		begin := time.Now()
		pid, _ := visit(t, spec, nil)
		worst = max(worst, time.Since(begin))
		if pid != active.PID {
			t.Fatalf("a request was served by pid %d while the new copy was starting, want the old active copy %d", pid, active.PID)
		}
	}
	if rec, _ := loadCopyRecord(spec, active.Copy); rec.PID != active.PID {
		t.Errorf("the old active copy was replaced while the new one was not ready (pid %d -> %d)", active.PID, rec.PID)
	}
	if rec, _ := loadCopyRecord(spec, warm.Copy); rec.PID == warm.PID {
		t.Errorf("the warm copy was not replaced")
	}
	if worst > 500*time.Millisecond {
		t.Errorf("a request waited %v with a ready copy serving", worst)
	}

	// And the rest of the rollout never makes a request wait either.
	worst = 0
	for end := time.Now().Add(12 * time.Second); time.Now().Before(end); time.Sleep(40 * time.Millisecond) {
		begin := time.Now()
		visit(t, spec, nil)
		worst = max(worst, time.Since(begin))
		a, _ := loadCopyRecord(spec, active.Copy)
		w, _ := loadCopyRecord(spec, warm.Copy)
		if a.PID != active.PID && w.PID != warm.PID && nrolesNow(spec) == [2]int{1, 1} {
			break
		}
	}
	if worst > 500*time.Millisecond {
		t.Errorf("a request waited %v during the rollout", worst)
	}
	waitRoles(t, spec, 1, 1)
}

func nrolesNow(spec manifest.AppSpec) [2]int {
	p, _ := gateway.Pool(spec.AppStateFile)
	return [2]int{len(p.Active), len(p.Warm)}
}

// A copy an earlier daemon started, still starting when this daemon looks at
// it, is promoted as soon as it listens, without waiting for another cycle.
func TestAdopted_StartingCopyGetsAReadyWatcher(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	spec.Env = map[string]string{"FAKE_RSCRIPT_DELAY": "800ms"}
	cleanupApp(t, spec)
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	rec, _ := startRaw(t, spec, 1)
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{rec}}); err != nil {
		t.Fatal(err)
	}

	// One cycle, the way a tick does it: the copy is alive, not listening yet.
	reconcileApp(spec.AppStateFile)
	if p, _ := gateway.Pool(spec.AppStateFile); len(p.Active) != 0 {
		t.Fatalf("pool = %+v before the copy listens", p)
	}
	// No second cycle is run here: the watcher does it.
	waitRoles(t, spec, 1, 0)
}

// One probe of the reconcile's own that misses must not strand a copy that
// the ready-watcher saw listening.
func TestReadyWatcher_OneMissedProbeDoesNotStrandTheCopy(t *testing.T) {
	useFakeRscript(t)
	var missed atomic.Bool
	useHooks(t, hooks{ObserveProbe: func(host string, port int) bool {
		if missed.CompareAndSwap(false, true) {
			return false
		}
		return process.IsReady(host, port)
	}})
	spec := copiesSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	applyOK(t, spec)
	waitRoles(t, spec, 1, 0)
	if !missed.Load() {
		t.Error("the probe was never missed: the test did not test anything")
	}
}

// Status calls a copy ready only when it accepts connections and the load
// balancer lists it.
func TestHandleStatus_ReadyNeedsTheCopyListed(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	applyOK(t, spec)
	waitRoles(t, spec, 1, 0)
	if c := statusOf(t, "app").Copies[0]; !c.Ready || c.Role != "active" {
		t.Fatalf("copy = %+v, want ready and active", c)
	}

	// Alive and accepting, but not in the pool.
	gateway.SetPool(spec.AppStateFile, proxy.Pool{})
	if c := statusOf(t, "app").Copies[0]; c.Ready || c.Role != "" {
		t.Errorf("copy = %+v after it left the pool, want not ready and no role", c)
	}
	refreshPools()
	if c := statusOf(t, "app").Copies[0]; !c.Ready {
		t.Errorf("copy = %+v after the pool was rebuilt, want ready", c)
	}
}

// refreshPools only looks: it doesn't mark anything seen, count an exit or
// change a record.
func TestRefreshPools_HasNoSideEffects(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	one, two := adoptCopies(t, spec) // calls refreshPools itself
	for _, rec := range loadCopies(t, spec) {
		if wasSeen(spec.AppStateFile, rec) {
			t.Errorf("copy %d was marked seen by refreshPools", rec.Copy)
		}
	}

	// A copy that died unseen: only a tick that had seen it counts the exit.
	reconcileApp(spec.AppStateFile) // sees both
	syscall.Kill(two.PID, syscall.SIGKILL)
	waitGone(t, two)
	before := loadCopies(t, spec)
	refreshPools()
	after := loadCopies(t, spec)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("refreshPools changed the records:\n%+v\n%+v", before, after)
	}
	for _, rec := range after {
		if rec.Copy == 2 && (rec.CrashCount != 0 || !rec.LastExitAt.IsZero()) {
			t.Errorf("refreshPools counted the exit of copy 2: %+v", rec)
		}
	}
	_ = one
	// The tick still counts it afterwards, which it could not if refreshPools
	// had forgotten that it was seen.
	reconcileApp(spec.AppStateFile)
	if rec, _ := loadCopyRecord(spec, 2); rec.CrashCount != 1 {
		t.Errorf("copy 2 = %+v, want its exit counted once by the tick", rec)
	}
}

// kill(pid, 0) saying EPERM means the pid is another user's process now: the
// ready-watcher stops, and doesn't trigger anything for it.
func TestReadyWatcher_StopsWhenThePidIsAnotherUsers(t *testing.T) {
	var probed atomic.Int32
	useHooks(t, hooks{
		Signal0:    func(int) error { return syscall.EPERM },
		WatchProbe: func(string, int) bool { probed.Add(1); return true },
	})
	const pid = 987654
	done := make(chan struct{})
	go func() { watchReady("/no/such/state.json", pid, 1); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the watcher is still running")
	}
	if probed.Load() != 0 {
		t.Errorf("the port was probed %d times for a process that is not ours", probed.Load())
	}
	if _, still := readyWatched.Load(pid); still {
		t.Error("the watcher's slot was not freed")
	}
}

// A new version that fails at start stalls the rollout also when the number of
// copies is lowered meanwhile: the one old copy that is ready is not replaced
// by the broken version, and status says why it stays.
func TestApply_BrokenVersionDuringAScaleDownStalls(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active := copyByDial(t, spec, pool.Active[0])

	broken := spec
	broken.Env = map[string]string{"FAKE_RSCRIPT_EXIT": "1"}
	applyResponse(t, broken) // replaces the warm copy, which dies at once

	// ... and now the number of copies is lowered to one.
	single := broken
	zero := 0
	single.WarmReplicas = &zero
	resp := applyResponse(t, single)
	if strings.Contains(resp.Message, "stopped the previous instance") {
		t.Fatalf("the old copy was replaced by the broken version: %q", resp.Message)
	}

	stop := tick(single)
	defer stop()
	end := time.Now().Add(4 * time.Second)
	for time.Now().Before(end) {
		if pid, _ := visit(t, spec, nil); pid != active.PID {
			t.Fatalf("served by pid %d, want the old copy %d", pid, active.PID)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rec, _ := loadCopyRecord(spec, active.Copy); rec.PID != active.PID || !process.IsAlive(spec, rec) {
		t.Errorf("the old copy was replaced or stopped: %+v", rec)
	}
	if got := len(loadCopies(t, spec)); got != 2 {
		t.Errorf("%d copies recorded, want the old one and the broken one still being retried", got)
	}

	var out bytes.Buffer
	if err := writeStatus(&out, httptestStatus(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "app: copy 1 keeps serving the previous version: copy 2 does not start (see shinyctl logs --copy 2)") {
		t.Errorf("status does not say why copy 1 stays:\n%s", out.String())
	}
}
