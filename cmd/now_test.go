/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
)

// postJSONTo is postJSON with a request target, such as "/?now=1".
func postJSONTo(t *testing.T, handler http.HandlerFunc, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, target, bytes.NewReader(data)))
	return rec
}

// restart --now: the old copy stops as soon as the new one is ready, with its
// visitors, and with no tick to do it.
func TestRedeploy_NowStopsTheOldCopyOnceTheNewIsReady(t *testing.T) {
	useFakeRscript(t)
	spec := slowSpec(t, 1, 0, "600ms")
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	old := copyByDial(t, spec, pool.Active[0])
	closeIt, _ := holdWith(t, spec, nil)
	waitUntil(t, "the session", 5*time.Second, func() bool { return gateway.Sessions()[pool.Active[0]] == 1 })

	rec := postJSONTo(t, handleRestart, "/?now=1", spec)
	if rec.Code != http.StatusOK {
		t.Fatalf("restart: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if msg := decodeApplyResponse(t, rec).Message; !strings.HasSuffix(msg, "then stops") {
		t.Errorf("message = %q, want it to end with \"then stops\"", msg)
	}
	if cur, _ := loadCopyRecord(spec, old.Copy); !cur.SkipDrain {
		t.Errorf("copy %d = %+v, want skip-drain set", old.Copy, cur)
	}
	waitUntil(t, "the old copy to stop without any tick", 10*time.Second, func() bool { return !process.IsAlive(spec, old) })
	closeIt()
	waitPool(t, spec, "the new copy to serve alone", func(p proxy.Pool) bool {
		return len(p.Active) == 1 && p.Active[0] != pool.Active[0] && len(p.Draining) == 0
	})
	waitUntil(t, "the old copy's record to go", 5*time.Second, func() bool {
		cur, ok := loadCopyRecord(spec, old.Copy)
		return !ok || cur.PID != old.PID
	})
}

// apply --now ends a drain in progress at once.
func TestRedeploy_NowEndsADrainInProgress(t *testing.T) {
	useFakeRscript(t)
	r := startRedeploy(t, copiesSpec(t, "app", 1, 0))
	rec := postJSONTo(t, handleApply, "/?now=1", r.spec)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if want := fmt.Sprintf(`Updated %q (stopping draining copy %d)`, r.spec.AppName, r.old.Copy); decodeApplyResponse(t, rec).Message != want {
		t.Errorf("message = %q, want %q", decodeApplyResponse(t, rec).Message, want)
	}
	if process.IsAlive(r.spec, r.old) {
		t.Error("the draining copy is still running after apply --now")
	}
}

// The mark outlives the request: a daemon that restarts (the table rebuilt
// from the state files) between the request and the switch still stops the
// old copy at the switch instead of draining it.
func TestRedeploy_NowMarkOutlivesTheRequest(t *testing.T) {
	useFakeRscript(t)
	buf := captureLog(t)
	spec := slowSpec(t, 1, 0, "1500ms")
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	old := copyByDial(t, spec, pool.Active[0])
	holdWith(t, spec, nil)

	if rec := postJSONTo(t, handleRestart, "/?now=1", spec); rec.Code != http.StatusOK {
		t.Fatalf("restart: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	seq := gateway.PoolSeq()
	gateway.ReplacePools(proxy.Table{}, seq)
	refreshPools()
	if got, _ := gateway.Pool(spec.AppStateFile); !slices.Equal(got.Active, []string{pool.Active[0]}) {
		t.Fatalf("pool = %+v after the rebuild, want the old copy still serving", got)
	}
	waitUntil(t, "the old copy to stop", 10*time.Second, func() bool { return !process.IsAlive(spec, old) })
	if !strings.Contains(buf.String(), fmt.Sprintf("stopping copy %d (pid %d) without draining", old.Copy, old.PID)) {
		t.Errorf("log lacks the no-drain line:\n%s", buf.String())
	}
}

// A scale-down with --now stops the copy it removes before the reply.
func TestScaleDown_NowStopsAtOnce(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 2, 0)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 2, 0)
	holdWith(t, spec, nil)
	holdWith(t, spec, nil)
	waitUntil(t, "two sessions", 5*time.Second, func() bool {
		s := gateway.Sessions()
		return s[pool.Active[0]] == 1 && s[pool.Active[1]] == 1
	})
	before := loadCopies(t, spec)

	smaller := spec
	smaller.Replicas = 1
	rec := postJSONTo(t, handleApply, "/?now=1", smaller)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	msg := decodeApplyResponse(t, rec).Message
	prefix := fmt.Sprintf("Updated %q without a restart (manifest changed: replicas; stopping copy ", spec.AppName)
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(msg, prefix), ")"))
	if err != nil || !strings.HasPrefix(msg, prefix) {
		t.Fatalf("message = %q", msg)
	}
	for _, rec := range before {
		if rec.Copy == n && process.IsAlive(spec, rec) {
			t.Errorf("copy %d is still running when the reply came", n)
		}
		if rec.Copy != n && !process.IsAlive(spec, rec) {
			t.Errorf("copy %d was stopped too", rec.Copy)
		}
	}
	// The copy that stays is of the current version: it carries no mark, so a
	// later redeploy drains it.
	for _, rec := range loadCopies(t, spec) {
		if rec.SkipDrain {
			t.Errorf("copy %d carries skip-drain: %+v", rec.Copy, rec)
		}
	}
}

// After a rollout with --now settles no copy carries the mark, and the next
// ordinary restart drains.
func TestRedeploy_NowLeavesTheNewCopiesUnmarked(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 0)
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	if rec := postJSONTo(t, handleRestart, "/?now=1", spec); rec.Code != http.StatusOK {
		t.Fatalf("restart: status = %d", rec.Code)
	}
	waitPool(t, spec, "the new copy alone", func(p proxy.Pool) bool { return len(p.Active) == 1 && p.Active[0] != pool.Active[0] })
	waitUntil(t, "the old copy to be gone", 10*time.Second, func() bool { return len(loadCopies(t, spec)) == 1 })
	for _, rec := range loadCopies(t, spec) {
		if rec.SkipDrain {
			t.Errorf("copy %d carries skip-drain: %+v", rec.Copy, rec)
		}
	}

	holdWith(t, spec, nil)
	cur, _ := gateway.Pool(spec.AppStateFile)
	waitUntil(t, "the session", 5*time.Second, func() bool { return gateway.Sessions()[cur.Active[0]] == 1 })
	restartResponse(t, spec)
	waitPool(t, spec, "a normal restart drains", func(p proxy.Pool) bool { return slices.Contains(p.Draining, cur.Active[0]) })
}

// The command line has --now on apply and restart.
func TestCommands_HaveNow(t *testing.T) {
	for name, tc := range map[string]struct {
		cmd  *cobra.Command
		help string
	}{
		"apply":   {applyCmd, "stop copies that are replaced or no longer wanted without waiting for their visitors (also ends any drain in progress)"},
		"restart": {restartCmd, "stop the old copies as soon as the new ones are ready, without waiting for their visitors (also ends any drain in progress)"},
	} {
		flag := tc.cmd.Flags().Lookup("now")
		if flag == nil || flag.Value.Type() != "bool" || flag.Usage != tc.help {
			t.Errorf("%s --now = %+v, want a bool flag with the help %q", name, flag, tc.help)
		}
	}
}

// apply --now with a new env, then the old env put back before the new copy is
// ready (a change and its revert): the old copy is current again and loses
// the mark, so a later ordinary restart drains it instead of cutting its
// visitors.
func TestRedeploy_NowMarkIsClearedWhenTheOldVersionComesBack(t *testing.T) {
	useFakeRscript(t)
	spec := slowSpec(t, 1, 0, "2s")
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	old := copyByDial(t, spec, pool.Active[0])

	changed := spec
	changed.Env = map[string]string{"FAKE_RSCRIPT_DELAY": "2s", "X": "1"}
	if rec := postJSONTo(t, handleApply, "/?now=1", changed); rec.Code != http.StatusOK {
		t.Fatalf("apply --now: %d %s", rec.Code, rec.Body.String())
	}
	if cur, _ := loadCopyRecord(spec, old.Copy); !cur.SkipDrain {
		t.Fatalf("copy %d = %+v, want it marked by --now", old.Copy, cur)
	}
	if rec := postJSONTo(t, handleApply, "/", spec); rec.Code != http.StatusOK {
		t.Fatalf("apply back: %d %s", rec.Code, rec.Body.String())
	}
	if cur, _ := loadCopyRecord(spec, old.Copy); cur.PID != old.PID || cur.SkipDrain {
		t.Errorf("copy %d = %+v after the revert, want the same process without the mark", old.Copy, cur)
	}

	closeIt, _ := holdWith(t, spec, nil)
	defer closeIt()
	waitUntil(t, "the session", 5*time.Second, func() bool { return gateway.Sessions()[pool.Active[0]] == 1 })
	reply := restartResponse(t, spec).Message
	if strings.HasSuffix(reply, "then stops") {
		t.Errorf("an ordinary restart says %q: the old --now is still in force", reply)
	}
	waitPool(t, spec, "the old copy to drain", func(p proxy.Pool) bool { return slices.Contains(p.Draining, pool.Active[0]) })
	if !process.IsAlive(spec, old) {
		t.Errorf("copy %d was stopped with its visitor instead of draining", old.Copy)
	}
}
