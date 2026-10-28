/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/state"
)

const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"

// holdWith opens a /hold request, a session as far as the load balancer can
// tell, with the cookie if given, and returns once the headers came. It
// returns the cookie the load balancer set (nil when a cookie was sent) and
// a function that ends the session.
func holdWith(t *testing.T, spec manifest.AppSpec, cookie *http.Cookie) (closeIt func(), set *http.Cookie) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+dialAddr(spec.EffectiveHost(), spec.Port)+"/hold", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		set = c
	}
	var once sync.Once
	closeIt = func() { once.Do(func() { resp.Body.Close() }) }
	t.Cleanup(closeIt)
	return closeIt, set
}

// visitWith is visit with an Accept header, as a page load or a fetch has.
func visitWith(t *testing.T, spec manifest.AppSpec, cookie *http.Cookie, accept string) (pid int, set *http.Cookie) {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+dialAddr(spec.EffectiveHost(), spec.Port)+"/", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	req.Header.Set("Accept", accept)
	resp, err := (&http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", req.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	pid, err = strconv.Atoi(strings.TrimPrefix(string(body), "pid="))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, body %q", req.URL, resp.StatusCode, body)
	}
	for _, c := range resp.Cookies() {
		set = c
	}
	return pid, set
}

// waitPool waits until the load balancer's pool for the app satisfies cond.
func waitPool(t *testing.T, spec manifest.AppSpec, what string, cond func(proxy.Pool) bool) proxy.Pool {
	t.Helper()
	var pool proxy.Pool
	waitUntil(t, what, 10*time.Second, func() bool {
		pool, _ = gateway.Pool(spec.AppStateFile)
		return cond(pool)
	})
	return pool
}

// settleNow makes a draining copy with no sessions stoppable at the next
// cycle, instead of ten seconds on.
func settleNow(t *testing.T) { useHooks(t, hooks{DrainSettle: time.Nanosecond}) }

// waitStopped runs the cycles a tick would, until copy rec is stopped and its
// record gone.
func waitStopped(t *testing.T, spec manifest.AppSpec, rec state.AppState) {
	t.Helper()
	waitUntil(t, fmt.Sprintf("copy %d to be stopped", rec.Copy), 15*time.Second, func() bool {
		reconcileApp(spec.AppStateFile)
		cur, ok := loadCopyRecord(spec, rec.Copy)
		return !process.IsAlive(spec, rec) && (!ok || cur.PID != rec.PID)
	})
}

// newCopies are the records whose process isn't one of old.
func newCopies(t *testing.T, spec manifest.AppSpec, old ...state.AppState) []state.AppState {
	t.Helper()
	var out []state.AppState
	for _, rec := range loadCopies(t, spec) {
		if !slices.ContainsFunc(old, func(o state.AppState) bool { return o.PID == rec.PID }) {
			out = append(out, rec)
		}
	}
	return out
}

// slowSpec is an app whose copies take a while to listen.
func slowSpec(t *testing.T, replicas, warm int, delay string) manifest.AppSpec {
	spec := copiesSpec(t, "app", replicas, warm)
	spec.Env = map[string]string{"FAKE_RSCRIPT_DELAY": delay}
	return spec
}

// redeployed is an app of one copy and no spare whose copy 1 holds a session,
// restarted: copy 2 is the new version, which has taken over once copy 1 is
// draining.
type redeployed struct {
	spec    manifest.AppSpec
	old     state.AppState
	cookie  *http.Cookie // pins a visitor to the old copy
	closeIt func()
}

func startRedeploy(t *testing.T, spec manifest.AppSpec) redeployed {
	t.Helper()
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, spec.EffectiveWarm())
	old := copyByDial(t, spec, pool.Active[0])
	closeIt, cookie := holdWith(t, spec, nil)
	waitUntil(t, "the session to be counted", 5*time.Second, func() bool { return gateway.Sessions()[pool.Active[0]] == 1 })
	restartResponse(t, spec)
	waitPool(t, spec, "the switch", func(p proxy.Pool) bool { return slices.Contains(p.Draining, pool.Active[0]) })
	return redeployed{spec, old, cookie, closeIt}
}

// 1: a restart starts the copies that serve, next to the running ones, and
// returns at once; the old copies keep their roles.
func TestRedeploy_RestartStartsNewCopyAndReturnsAtOnce(t *testing.T) {
	useFakeRscript(t)
	spec := slowSpec(t, 1, 1, "600ms")
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	oldActive, oldWarm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])
	reloads := gateway.Reloads()

	resp := restartResponse(t, spec)
	if want := `Restarting app "app": starting copy 3; copies 1 and 2 keep serving until the new one is ready, then drain for up to 15m`; resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}
	for _, rec := range []state.AppState{oldActive, oldWarm} {
		if !process.IsAlive(spec, rec) {
			t.Errorf("copy %d (pid %d) is not running", rec.Copy, rec.PID)
		}
	}
	recs := loadCopies(t, spec)
	if len(recs) != 3 {
		t.Fatalf("records = %+v, want the two old copies and one new one (the spare starts after the switch)", recs)
	}
	ports := map[int]bool{}
	for _, rec := range recs {
		ports[rec.Port] = true
	}
	if len(ports) != 3 {
		t.Errorf("internal ports %v, want three different ones", ports)
	}
	if got, _ := gateway.Pool(spec.AppStateFile); !slices.Equal(got.Active, []string{dialOf(oldActive)}) || !slices.Equal(got.Warm, []string{dialOf(oldWarm)}) || len(got.Draining) != 0 {
		t.Errorf("pool = %+v right after the reply, want the old copies serving", got)
	}
	if got := gateway.Reloads(); got != reloads {
		t.Errorf("the load balancer was reloaded %d time(s)", got-reloads)
	}
}

// 2: once the new copy is ready it takes the new visitors; the old active copy
// drains, the old spare, which has no visitors, stops at once, and the new
// spare starts.
func TestRedeploy_SwitchesOnceTheNewCopyIsReady(t *testing.T) {
	useFakeRscript(t)
	spec := slowSpec(t, 1, 1, "300ms")
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	oldActive, oldWarm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])
	reloads := gateway.Reloads()
	holdWith(t, spec, nil) // a session on the old active copy keeps it draining
	waitUntil(t, "the session", 5*time.Second, func() bool { return gateway.Sessions()[pool.Active[0]] == 1 })

	restartResponse(t, spec)
	got := waitPool(t, spec, "the switch", func(p proxy.Pool) bool {
		return len(p.Active) == 1 && p.Active[0] != pool.Active[0] && len(p.Draining) == 1
	})
	if got.Draining[0] != dialOf(oldActive) {
		t.Errorf("pool = %+v, want the old active copy draining", got)
	}
	if pid, _ := visit(t, spec, nil); pid == oldActive.PID || pid == oldWarm.PID {
		t.Errorf("a new visitor was served by an old copy (pid %d)", pid)
	}
	waitUntil(t, "the old spare, which has no visitors, to stop at the switch", 5*time.Second, func() bool { return !process.IsAlive(spec, oldWarm) })
	if rec, _ := loadCopyRecord(spec, oldActive.Copy); rec.DrainingSince.IsZero() {
		t.Errorf("the old active copy has no draining-since: %+v", rec)
	}
	// The new spare starts after the switch and joins as warm.
	waitPool(t, spec, "the new spare", func(p proxy.Pool) bool { return len(p.Warm) == 1 && len(p.Active) == 1 })
	if got := gateway.Reloads(); got != reloads {
		t.Errorf("the load balancer was reloaded %d time(s)", got-reloads)
	}
}

// 2b: with one copy and one spare, a restart never runs more than three
// processes at once, a page held open on the old copy included.
func TestRedeploy_NeverMoreThanThreeProcessesWithTheDefaults(t *testing.T) {
	useFakeRscript(t)
	spec := slowSpec(t, 1, 1, "300ms")
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	holdWith(t, spec, nil)
	waitUntil(t, "the session", 5*time.Second, func() bool { return gateway.Sessions()[pool.Active[0]] == 1 })

	var peak int
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(3 * time.Millisecond):
			}
			st, err := state.Load(spec.AppStateFile)
			if err != nil {
				continue
			}
			n := 0
			for _, rec := range st.Copies() {
				if process.IsAlive(spec, rec) {
					n++
				}
			}
			peak = max(peak, n)
		}
	}()
	restartResponse(t, spec)
	waitPool(t, spec, "the switch", func(p proxy.Pool) bool { return len(p.Draining) == 1 && len(p.Active) == 1 })
	waitPool(t, spec, "the new spare", func(p proxy.Pool) bool { return len(p.Warm) == 1 })
	time.Sleep(200 * time.Millisecond)
	close(stop)
	<-done
	if peak != 3 {
		t.Errorf("the most processes at once = %d, want 3", peak)
	}
}

// 3: copies with no sessions stop after the switch, the old active one once it
// has settled.
func TestRedeploy_OldCopiesWithoutSessionsStop(t *testing.T) {
	useFakeRscript(t)
	settleNow(t)
	buf := captureLog(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	oldActive, oldWarm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	restartResponse(t, spec)
	waitPool(t, spec, "the switch", func(p proxy.Pool) bool { return len(p.Active) == 1 && p.Active[0] != pool.Active[0] })
	waitStopped(t, spec, oldWarm)
	waitStopped(t, spec, oldActive)
	log := buf.String()
	if !strings.Contains(log, fmt.Sprintf("stopping copy %d (pid %d): it has drained", oldActive.Copy, oldActive.PID)) {
		t.Errorf("log lacks the drained line for the old active copy:\n%s", log)
	}
	if !strings.Contains(log, fmt.Sprintf("stopping copy %d (pid %d): the new version has taken over and it has no visitors", oldWarm.Copy, oldWarm.PID)) {
		t.Errorf("log lacks the idle line for the old spare:\n%s", log)
	}
	waitPool(t, spec, "the new copies serve", func(p proxy.Pool) bool { return len(p.Active) == 1 && len(p.Warm) == 1 && len(p.Draining) == 0 })
}

// 4: a session keeps its copy; the old page's own requests stay on it, and a
// page load goes to the new version.
func TestRedeploy_ASessionKeepsItsCopy(t *testing.T) {
	useFakeRscript(t)
	settleNow(t)
	r := startRedeploy(t, copiesSpec(t, "app", 1, 0))

	reconcileApp(r.spec.AppStateFile)
	if rec, _ := loadCopyRecord(r.spec, r.old.Copy); rec.PID != r.old.PID || !process.IsAlive(r.spec, rec) || rec.DrainingSince.IsZero() {
		t.Fatalf("the old copy with a session is %+v, want it alive and draining", rec)
	}
	if pid, set := visitWith(t, r.spec, r.cookie, "*/*"); pid != r.old.PID || set != nil {
		t.Errorf("a fetch of the old page went to pid %d with cookie %v, want the old copy %d and no cookie", pid, set, r.old.PID)
	}
	pid, set := visitWith(t, r.spec, r.cookie, browserAccept)
	if pid == r.old.PID || set == nil {
		t.Errorf("a page load went to pid %d with cookie %v, want the new version and a cookie for it", pid, set)
	}
	if pid2, set2 := visitWith(t, r.spec, set, browserAccept); pid2 != pid || set2 != nil {
		t.Errorf("the visitor moved again: pid %d -> %d", pid, pid2)
	}

	r.closeIt()
	waitStopped(t, r.spec, r.old)
}

// 5: drain-timeout ends a drain that has sessions, and cuts them.
func TestRedeploy_DrainTimeoutCutsTheSession(t *testing.T) {
	useFakeRscript(t)
	settleNow(t)
	buf := captureLog(t)
	spec := copiesSpec(t, "app", 1, 0)
	spec.DrainTimeout = "1s"
	r := startRedeploy(t, spec)
	reconcileApp(spec.AppStateFile)
	if !process.IsAlive(spec, r.old) {
		t.Fatal("the old copy stopped before drain-timeout")
	}
	time.Sleep(1100 * time.Millisecond)
	waitStopped(t, spec, r.old)
	if !strings.Contains(buf.String(), "drain-timeout passed with 1 session(s) open") {
		t.Errorf("log lacks the timeout line:\n%s", buf.String())
	}
}

// 6: drain-timeout 0: the old copy stops at the switch, from the cycle the
// ready-watcher runs.
func TestRedeploy_DrainTimeoutZeroStopsAtTheSwitch(t *testing.T) {
	useFakeRscript(t)
	spec := slowSpec(t, 1, 0, "300ms")
	spec.DrainTimeout = "0"
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	old := copyByDial(t, spec, pool.Active[0])
	holdWith(t, spec, nil)

	resp := restartResponse(t, spec)
	if want := `Restarting app "app": starting copy 2; copy 1 keeps serving until the new one is ready, then stops`; resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}
	waitUntil(t, "the old copy to stop without any tick", 10*time.Second, func() bool { return !process.IsAlive(spec, old) })
	waitPool(t, spec, "the new copy to serve", func(p proxy.Pool) bool {
		return len(p.Active) == 1 && p.Active[0] != pool.Active[0] && len(p.Draining) == 0
	})
}

// 7: nobody waits for R: the old copy serves until the new one is ready.
func TestRedeploy_NoRequestWaits(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	a := applyOK(t, spec)
	waitServed(t, spec, a.PID)

	var mu sync.Mutex
	var pids []int
	var failures []string
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		client := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			resp, err := client.Get("http://" + dialAddr(spec.EffectiveHost(), spec.Port) + "/")
			mu.Lock()
			if err != nil {
				failures = append(failures, err.Error())
			} else {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				pid, _ := strconv.Atoi(strings.TrimPrefix(string(body), "pid="))
				if resp.StatusCode != http.StatusOK {
					failures = append(failures, fmt.Sprintf("status %d", resp.StatusCode))
				}
				pids = append(pids, pid)
			}
			mu.Unlock()
		}
	}()

	next := spec
	next.Env = map[string]string{"GREETING": "again", "FAKE_RSCRIPT_DELAY": "700ms"}
	applyResponse(t, next)
	waitPool(t, spec, "the switch", func(p proxy.Pool) bool { return len(p.Active) == 1 && len(p.Draining) == 1 })
	time.Sleep(300 * time.Millisecond)
	close(stop)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(failures) > 0 {
		t.Errorf("%d request(s) failed or waited: %v", len(failures), failures)
	}
	if len(pids) < 10 || pids[0] != a.PID || pids[len(pids)-1] == a.PID {
		t.Errorf("served by %v, want the old pid %d and then a new one", pids, a.PID)
	}
}

// 8: a new version that dies at start never takes over.
func TestRedeploy_BrokenVersionStalls(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	oldActive, oldWarm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	broken := spec
	broken.Env = map[string]string{"FAKE_RSCRIPT_EXIT": "1"}
	applyResponse(t, broken)

	stopTicks := tick(broken)
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		if pid, _ := visit(t, spec, nil); pid != oldActive.PID {
			t.Fatalf("served by pid %d, want the old active copy %d", pid, oldActive.PID)
		}
		if got, _ := gateway.Pool(spec.AppStateFile); len(got.Draining) != 0 || !slices.Equal(got.Active, []string{pool.Active[0]}) || !slices.Equal(got.Warm, []string{pool.Warm[0]}) {
			t.Fatalf("pool = %+v, want the old copies serving and nothing draining", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stopTicks()
	for _, rec := range []state.AppState{oldActive, oldWarm} {
		if cur, _ := loadCopyRecord(spec, rec.Copy); cur.PID != rec.PID || !cur.DrainingSince.IsZero() || !process.IsAlive(spec, cur) {
			t.Errorf("copy %d = %+v, want it untouched", rec.Copy, cur)
		}
	}
	var out bytes.Buffer
	if err := writeStatus(&out, httptestStatus(t)); err != nil {
		t.Fatal(err)
	}
	if want := "app: copies 1 and 2 keep serving the previous version: copy 3 does not start (see shinyctl logs --copy 3)"; !strings.Contains(out.String(), want) {
		t.Errorf("status lacks %q:\n%s", want, out.String())
	}

	// The manifest as it was: the broken copy goes, nothing drains.
	applyResponse(t, spec)
	waitUntil(t, "the broken copy to go", 5*time.Second, func() bool {
		reconcileApp(spec.AppStateFile)
		recs := loadCopies(t, spec)
		return len(recs) == 2 && recs[0].PID == oldActive.PID && recs[1].PID == oldWarm.PID
	})
	for _, rec := range loadCopies(t, spec) {
		if !rec.DrainingSince.IsZero() {
			t.Errorf("copy %d is draining: %+v", rec.Copy, rec)
		}
	}
}

// 9: an apply during a rollout goes straight to the newest version.
func TestRedeploy_ApplyDuringARollout(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)

	first := spec
	first.Env = map[string]string{"A": "1", "FAKE_RSCRIPT_DELAY": "3s"}
	applyResponse(t, first)
	second := spec
	second.Env = map[string]string{"A": "2", "FAKE_RSCRIPT_DELAY": "300ms"}
	resp := applyResponse(t, second)
	for _, want := range []string{"manifest changed: env", "starting copy 4", "stopping copy 3"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("message %q lacks %q", resp.Message, want)
		}
	}
	got := waitPool(t, spec, "the switch to the newest", func(p proxy.Pool) bool { return len(p.Active) == 1 && p.Active[0] != pool.Active[0] })
	if rec := copyByDialRecord(t, spec, 4); dialOf(rec) != got.Active[0] {
		t.Errorf("active = %v, want copy 4 (%s)", got.Active, dialOf(rec))
	}
}

// 10: scaling the spare away in the middle of a rollout stops the old spare
// (it has no visitors); one with a visitor would drain.
func TestRedeploy_ScaleDownDuringARollout(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)

	slow := spec
	slow.Env = map[string]string{"A": "1", "FAKE_RSCRIPT_DELAY": "1500ms"}
	applyResponse(t, slow)
	smaller := slow
	zero := 0
	smaller.WarmReplicas = &zero
	resp := applyResponse(t, smaller)
	if !strings.Contains(resp.Message, "stopping copy 2") {
		t.Errorf("message = %q, want the old spare, which has no visitors, stopped", resp.Message)
	}
	if got, _ := gateway.Pool(spec.AppStateFile); !slices.Equal(got.Active, []string{pool.Active[0]}) || len(got.Warm) != 0 || len(got.Draining) != 0 {
		t.Errorf("pool = %+v, want the old active copy serving alone", got)
	}
	waitPool(t, spec, "the switch", func(p proxy.Pool) bool { return len(p.Active) == 1 && p.Active[0] != pool.Active[0] })
}

// 11: a draining copy that dies is dropped, not started again, and its
// visitors go to the new version.
func TestRedeploy_DrainingCopyThatDiesIsNotStartedAgain(t *testing.T) {
	useFakeRscript(t)
	r := startRedeploy(t, copiesSpec(t, "app", 1, 0))
	if err := syscall.Kill(r.old.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the record of the dead draining copy to go", 5*time.Second, func() bool {
		_, ok := loadCopyRecord(r.spec, r.old.Copy)
		return !ok
	})
	if recs := loadCopies(t, r.spec); len(recs) != 1 || recs[0].Copy == r.old.Copy {
		t.Errorf("records = %+v, want only the new copy", recs)
	}
	if pid, _ := visitWith(t, r.spec, r.cookie, "*/*"); pid == r.old.PID {
		t.Errorf("a visitor of the dead copy was served by pid %d", pid)
	}
}

// 12: stop ends a drain at once.
func TestRedeploy_StopEndsTheDrainAtOnce(t *testing.T) {
	useFakeRscript(t)
	r := startRedeploy(t, copiesSpec(t, "app", 1, 0))
	recs := loadCopies(t, r.spec)
	if rec := postJSON(t, handleStop, r.spec); rec.Code != http.StatusOK {
		t.Fatalf("stop: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, rec := range recs {
		if process.IsAlive(r.spec, rec) {
			t.Errorf("copy %d (pid %d) is still running after stop", rec.Copy, rec.PID)
		}
	}
	waitClosed(t, r.spec.Host, r.spec.Port)
}

// 13: a daemon that rebuilds the table from the state files lists the
// draining copies.
func TestRedeploy_RefreshPoolsKeepsDraining(t *testing.T) {
	useFakeRscript(t)
	r := startRedeploy(t, copiesSpec(t, "app", 1, 0))
	gateway.ReplacePools(proxy.Table{}, gateway.PoolSeq())
	refreshPools()
	pool, _ := gateway.Pool(r.spec.AppStateFile)
	if !slices.Equal(pool.Draining, []string{dialOf(r.old)}) || len(pool.Active) != 1 {
		t.Errorf("pool = %+v, want the old copy draining and one active", pool)
	}
}

// 14: after a reboot a draining record is dropped and never started; the
// current ones start in place; nothing is counted as a crash.
func TestRedeploy_AfterARebootDrainingCopiesStayGone(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	one, _ := startRaw(t, spec, 1)
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{one}}); err != nil {
		t.Fatal(err)
	}
	two, _ := startRaw(t, spec, 2)
	one.DrainingSince = time.Now().Add(-time.Minute)
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{one, two}}); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []state.AppState{one, two} {
		syscall.Kill(rec.PID, syscall.SIGKILL)
		waitGone(t, rec)
		forgetSeen(spec.AppStateFile, rec.Copy)
	}

	reconcileApp(spec.AppStateFile)
	recs := loadCopies(t, spec)
	if len(recs) != 1 || recs[0].Copy != 2 || recs[0].PID == two.PID || recs[0].PID == 0 {
		t.Fatalf("records = %+v, want copy 2 started again and copy 1 gone", recs)
	}
	if recs[0].CrashCount != 0 || !recs[0].LastExitAt.IsZero() {
		t.Errorf("copy 2 = %+v, want no crash counted", recs[0])
	}
}

// 15: a rollout from a phase-2 build caught half done (copy 1 at the old
// revision, copy 2 at the new) switches at once: copy 1 drains.
func TestRedeploy_UpgradeFromAPhase2RolloutHalfDone(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	startAt := func(n, revision int) state.AppState {
		port, err := process.PickPort(takenPorts(spec.AppStateFile, n), func(p int) bool { return process.PortAvailable(process.InternalHost, p) == nil }, currentHooks().PortStart())
		if err != nil {
			t.Fatal(err)
		}
		rec, _, err := process.Start(spec, n, port, revision)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	one := startAt(1, 0)
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Revision: 1, Apps: []state.AppState{one}}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, process.InternalHost, one.Port)
	two := startAt(2, 1)
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Revision: 1, Apps: []state.AppState{one, two}}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, process.InternalHost, two.Port)

	triggerReconcile(spec.AppStateFile)
	recs := loadCopies(t, spec)
	if len(recs) != 3 {
		t.Fatalf("records = %+v, want copy 3 started", recs)
	}
	if rec, _ := loadCopyRecord(spec, 1); rec.DrainingSince.IsZero() {
		t.Errorf("copy 1 = %+v, want it draining", rec)
	}
	pool, _ := gateway.Pool(spec.AppStateFile)
	if !slices.Equal(pool.Active, []string{dialOf(two)}) || !slices.Equal(pool.Draining, []string{dialOf(one)}) {
		t.Errorf("pool = %+v, want copy 2 active and copy 1 draining", pool)
	}
}

// 16: a scale-down drains the copies it removes; one with a session stays
// until it ends.
func TestScaleDown_DrainsTheSurplus(t *testing.T) {
	useFakeRscript(t)
	settleNow(t)
	spec := copiesSpec(t, "app", 2, 0)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 2, 0)

	close1, _ := holdWith(t, spec, nil)
	var first string
	waitUntil(t, "the first session", 5*time.Second, func() bool {
		for dial, n := range gateway.Sessions() {
			if n == 1 && slices.Contains(pool.Active, dial) {
				first = dial
			}
		}
		return first != ""
	})
	close2, _ := holdWith(t, spec, nil)
	waitUntil(t, "the second session", 5*time.Second, func() bool {
		s := gateway.Sessions()
		return s[pool.Active[0]] == 1 && s[pool.Active[1]] == 1
	})

	smaller := spec
	smaller.Replicas = 1
	resp := applyResponse(t, smaller)
	m := strings.TrimPrefix(resp.Message, fmt.Sprintf("Updated %q without a restart (manifest changed: replicas; draining copy ", spec.AppName))
	n, err := strconv.Atoi(strings.TrimSuffix(m, ")"))
	if err != nil || m == resp.Message {
		t.Fatalf("message = %q", resp.Message)
	}
	gone := copyByDialRecord(t, spec, n)
	reconcileApp(spec.AppStateFile)
	if !process.IsAlive(spec, gone) {
		t.Fatalf("copy %d holds a session and was stopped", n)
	}
	if first == dialOf(gone) {
		close1()
	} else {
		close2()
	}
	waitStopped(t, spec, gone)
	for _, rec := range loadCopies(t, spec) {
		if rec.Copy == n {
			t.Errorf("copy %d is still recorded", n)
		}
	}
}

// 17: a spare that is scaled away drains and, having no sessions, stops; the
// active copy is untouched.
func TestScaleDown_TheSpareDrainsAndStops(t *testing.T) {
	useFakeRscript(t)
	settleNow(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active, warm := copyByDial(t, spec, pool.Active[0]), copyByDial(t, spec, pool.Warm[0])

	none := spec
	zero := 0
	none.WarmReplicas = &zero
	resp := applyResponse(t, none)
	if want := fmt.Sprintf("Updated %q without a restart (manifest changed: warm-replicas; draining copy %d)", spec.AppName, warm.Copy); resp.Message != want {
		t.Errorf("message = %q, want %q", resp.Message, want)
	}
	waitStopped(t, none, warm)
	if rec, _ := loadCopyRecord(spec, active.Copy); rec.PID != active.PID || !process.IsAlive(spec, rec) {
		t.Errorf("the active copy changed: %+v", rec)
	}
	waitRoles(t, none, 1, 0)
	if pid, _ := visit(t, spec, nil); pid != active.PID {
		t.Errorf("served by pid %d, want %d", pid, active.PID)
	}
}

// 18: status shows a draining copy, its sessions and the time it has left.
func TestHandleStatus_Draining(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 0)
	spec.DrainTimeout = "20m"
	r := startRedeploy(t, spec)
	resp := httptestStatus(t)
	var found bool
	for _, a := range resp.Apps {
		if a.Name != "app" {
			continue
		}
		for _, c := range a.Copies {
			if c.Copy != r.old.Copy {
				continue
			}
			found = true
			rec, _ := loadCopyRecord(r.spec, c.Copy)
			if c.Role != "draining" || c.Sessions == nil || *c.Sessions != 1 || !c.Outdated {
				t.Errorf("draining copy = %+v, want role draining, 1 session, outdated", c)
			}
			if d := c.DrainEndsAt.Sub(rec.DrainingSince.Add(20 * time.Minute)); d < -2*time.Second || d > 2*time.Second {
				t.Errorf("DrainEndsAt = %v, want draining-since %v + 20m", c.DrainEndsAt, rec.DrainingSince)
			}
		}
	}
	if !found {
		t.Fatalf("no row for copy %d: %+v", r.old.Copy, resp.Apps)
	}
	var out bytes.Buffer
	if err := writeStatus(&out, resp); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"app: copy 1 is draining with 1 session; it stops when it ends, or in 20m at the latest.", "(1 draining)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
}

// 19: with the ticker running, the old copy serves every request within
// 500ms until the slow new copies are ready, and then the new ones do.
func TestRedeploy_SlowNewCopyIsWaitedFor(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 1)
	active := copyByDial(t, spec, pool.Active[0])

	slow := spec
	slow.Env = map[string]string{"FAKE_RSCRIPT_DELAY": "1500ms"}
	applyResponse(t, slow)
	stopTicks := tick(slow)
	defer stopTicks()

	var worst time.Duration
	for end := time.Now().Add(1200 * time.Millisecond); time.Now().Before(end); time.Sleep(40 * time.Millisecond) {
		begin := time.Now()
		pid, _ := visit(t, spec, nil)
		worst = max(worst, time.Since(begin))
		if pid != active.PID {
			t.Fatalf("a request was served by pid %d while the new copy was starting, want the old active copy %d", pid, active.PID)
		}
	}
	for end := time.Now().Add(12 * time.Second); time.Now().Before(end); time.Sleep(40 * time.Millisecond) {
		begin := time.Now()
		pid, _ := visit(t, spec, nil)
		worst = max(worst, time.Since(begin))
		if pid != active.PID {
			break
		}
	}
	if worst > 500*time.Millisecond {
		t.Errorf("a request waited %v during the redeploy", worst)
	}
	if pid, _ := visit(t, spec, nil); pid == active.PID {
		t.Errorf("the new version has not taken over")
	}
}

// drain-timeout over several cycles: draining-since is written once, when the
// drain begins, so the copy reaches its deadline however often a cycle runs.
func TestRedeploy_DrainingSinceIsWrittenOnceAndTheTimeoutIsReached(t *testing.T) {
	useFakeRscript(t)
	settleNow(t)
	spec := copiesSpec(t, "app", 1, 0)
	spec.DrainTimeout = "2s"
	r := startRedeploy(t, spec)
	first, _ := loadCopyRecord(spec, r.old.Copy)
	if first.DrainingSince.IsZero() {
		t.Fatalf("copy %d = %+v, want it draining", r.old.Copy, first)
	}
	for i := 0; i < 4; i++ {
		time.Sleep(300 * time.Millisecond)
		reconcileApp(spec.AppStateFile)
		cur, ok := loadCopyRecord(spec, r.old.Copy)
		if !ok || !process.IsAlive(spec, r.old) {
			t.Fatalf("cycle %d: the copy stopped before the deadline (%v after it began)", i, time.Since(first.DrainingSince))
		}
		if !cur.DrainingSince.Equal(first.DrainingSince) {
			t.Fatalf("cycle %d: draining-since %v, was %v: it is rewritten on every cycle", i, cur.DrainingSince, first.DrainingSince)
		}
	}
	time.Sleep(time.Until(first.DrainingSince.Add(2*time.Second + 100*time.Millisecond)))
	waitStopped(t, spec, r.old)
}

// While the load balancer can't say how many sessions a draining copy has, it
// is kept even with none; once it answers, the copy with none stops.
func TestRedeploy_DrainingCopyIsKeptWhileSessionsAreUnknown(t *testing.T) {
	useFakeRscript(t)
	var unknown atomic.Bool
	unknown.Store(true)
	useHooks(t, hooks{DrainSettle: time.Nanosecond, Sessions: func() map[string]int {
		if unknown.Load() {
			return nil
		}
		return gateway.Sessions()
	}})
	spec := copiesSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	old := copyByDial(t, spec, pool.Active[0])
	restartResponse(t, spec)
	waitPool(t, spec, "the switch", func(p proxy.Pool) bool { return slices.Contains(p.Draining, pool.Active[0]) })
	for i := 0; i < 5; i++ {
		reconcileApp(spec.AppStateFile)
		time.Sleep(100 * time.Millisecond)
	}
	if !process.IsAlive(spec, old) {
		t.Fatal("a draining copy was stopped for having no sessions while the load balancer could not say")
	}
	unknown.Store(false)
	waitStopped(t, spec, old)
}
