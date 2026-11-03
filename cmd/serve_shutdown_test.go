/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

// fakeShutdown is the handler's dependencies, recording what was called. The
// suite's load balancer is never stopped: its TestMain runs one for every
// test.
type fakeShutdown struct {
	calls      []string
	unit       string
	userUnit   bool
	balancerOK bool
	balancer   error
}

func (f *fakeShutdown) deps() shutdownDeps {
	return shutdownDeps{
		unit: func() (string, bool) {
			f.calls = append(f.calls, "unit")
			return f.unit, f.userUnit
		},
		stopLoops: func() { f.calls = append(f.calls, "stopLoops") },
		stopBalancer: func() (bool, error) {
			f.calls = append(f.calls, "stopBalancer")
			return f.balancerOK, f.balancer
		},
		exit: func() { f.calls = append(f.calls, "exit") },
	}
}

func (f *fakeShutdown) called() string { return strings.Join(f.calls, ",") }

// useShutdown returns fake dependencies, and registers a cleanup that clears
// shuttingDown. Call it after the test's apps have registered their own
// cleanups: cleanups run last to first, so the flag is clear when the apps
// are deleted, and the deletes' sync of the load balancer is not a no-op
// (otherwise the shared load balancer keeps the apps' ports). Between the
// reset and the delete a ready-watcher left from the test can start a copy of
// the app, which the delete then stops; expect that in the log.
func useShutdown(t *testing.T) *fakeShutdown {
	t.Helper()
	t.Cleanup(func() { shuttingDown.Store(false) })
	return &fakeShutdown{balancerOK: true}
}

// postShutdown calls the handler and decodes its answer.
func postShutdown(t *testing.T, f *fakeShutdown) (*httptest.ResponseRecorder, api.ShutdownResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	shutdownHandler(f.deps())(rec, httptest.NewRequest(http.MethodPost, "/shutdown", nil))
	var resp api.ShutdownResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decoding %q: %v", rec.Body.String(), err)
		}
	}
	return rec, resp
}

// appliedApp applies an app of its own, to be deleted when the test ends.
func appliedApp(t *testing.T, name string) (manifest.AppSpec, state.AppState) {
	t.Helper()
	spec := newTestSpec(t, t.TempDir(), name, "app.R", freePorts(t, 1)[0])
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
	})
	a = applyOK(t, spec)
	return spec, a
}

// mine is the apps of resp that the test started, in the order they came.
func mine(resp api.ShutdownResponse, names ...string) []string {
	var got []string
	for _, a := range resp.Apps {
		if slices.Contains(names, a) {
			got = append(got, a)
		}
	}
	return got
}

func TestHandleShutdown_StopsEveryAppAndTheBalancer(t *testing.T) {
	useFakeRscript(t)
	specB, b := appliedApp(t, "shutdown-beta")
	specA, a := appliedApp(t, "shutdown-alpha")
	f := useShutdown(t)

	rec, resp := postShutdown(t, f)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	for _, c := range []struct {
		spec manifest.AppSpec
		app  state.AppState
	}{{specA, a}, {specB, b}} {
		if process.IsAlive(c.spec, c.app) {
			t.Errorf("%s: process still running", c.spec.AppName)
		}
		if got := loadCopies(t, c.spec); len(got) != 0 {
			t.Errorf("%s: records left: %+v", c.spec.AppName, got)
		}
		if !appRegistry.Managed(c.spec.AppStateFile) {
			t.Errorf("%s: no longer managed; shutdown is power off, not delete", c.spec.AppName)
		}
	}
	if got := mine(resp, "shutdown-alpha", "shutdown-beta"); fmt.Sprint(got) != "[shutdown-alpha shutdown-beta]" {
		t.Errorf("Apps has %v (all: %v), want both, sorted", got, resp.Apps)
	}
	for _, p := range resp.Problems {
		if strings.Contains(p, "shutdown-") {
			t.Errorf("problem for the test's apps: %s", p)
		}
	}
	if want := "unit,stopLoops,stopBalancer,exit"; f.called() != want {
		t.Errorf("calls = %s, want %s", f.called(), want)
	}
	if !resp.Balancer {
		t.Error("Balancer = false, want true")
	}
	if resp.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", resp.PID, os.Getpid())
	}
}

func TestHandleShutdown_LeavesAReusedPIDAlone(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "shutdown-reused", "app.R", freePorts(t, 1)[0])
	t.Cleanup(func() { appRegistry.Forget(spec.AppStateFile) })

	a, done := startRaw(t, spec, 1)
	killIfAlive(spec, a)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake Rscript did not exit after SIGKILL")
	}
	pid, exited := startStranger(t)
	a.PID = pid
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{a}}); err != nil {
		t.Fatal(err)
	}
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	f := useShutdown(t)

	rec, resp := postShutdown(t, f)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	select {
	case <-exited:
		t.Fatalf("pid %d was signaled, but it is not this app's process", pid)
	default:
	}
	if syscall.Kill(pid, 0) != nil {
		t.Errorf("pid %d is gone", pid)
	}

	want := fmt.Sprintf("%q (recorded pid %d is not running this app, so it was not signaled)", spec.AppName, pid)
	if !slices.Contains(resp.Notes, want) {
		t.Errorf("Notes = %q, want %q among them", resp.Notes, want)
	}
	if len(mine(resp, spec.AppName)) != 0 {
		t.Errorf("Apps = %v names the app, but nothing of it was stopped", resp.Apps)
	}
}

func TestHandleShutdown_KeepsTheRecordOfACopyThatDidNotStop(t *testing.T) {
	recs := []state.AppState{{Copy: 1, PID: 100}, {Copy: 2, PID: 200}, {Copy: 3, PID: 300}}
	results := []copyStop{{signaled: true}, {err: errors.New("boom")}, {note: " (a note)"}}

	kept := keptRecords(recs, results)
	if len(kept) != 1 || kept[0].PID != 200 {
		t.Errorf("kept %+v, want only copy 2", kept)
	}
	if got := keptRecords(recs, make([]copyStop, 3)); len(got) != 0 {
		t.Errorf("kept %+v with no errors, want none", got)
	}
	if got := copyNumber(state.AppState{}, 1); got != 2 {
		t.Errorf("copyNumber of an unnumbered record in place 1 = %d, want 2", got)
	}
}

func TestHandleShutdown_Gates(t *testing.T) {
	useFakeRscript(t)
	running, _ := appliedApp(t, "shutdown-gated")
	idle := newTestSpec(t, t.TempDir(), "shutdown-idle", "app.R", freePorts(t, 1)[0])
	t.Cleanup(func() { appRegistry.Forget(idle.AppStateFile) })
	if err := appRegistry.Remember(idle); err != nil {
		t.Fatal(err)
	}
	fresh := newTestSpec(t, t.TempDir(), "shutdown-new", "app.R", freePorts(t, 1)[0])
	revision := func() int {
		st, err := state.Load(running.AppStateFile)
		if err != nil {
			t.Fatal(err)
		}
		return st.Revision
	}
	before := revision()

	f := useShutdown(t)
	shuttingDown.Store(true)

	if reconcileApp(idle.AppStateFile) {
		t.Error("reconcileApp started something while shutting down")
	}
	if got := loadCopies(t, idle); len(got) != 0 {
		t.Errorf("a copy was recorded for the idle app: %+v", got)
	}

	if _, err := reconcileLocked(idle, reconcileOpts{}); !errors.Is(err, errShuttingDown) {
		t.Errorf("reconcileLocked: err = %v, want errShuttingDown", err)
	}

	rec := postJSON(t, handleApply, fresh)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "shutting down") {
		t.Errorf("apply: status = %d, body = %s, want 503", rec.Code, rec.Body.String())
	}
	if _, known := appRegistry.Spec(fresh.AppStateFile); known {
		t.Error("apply remembered the app while shutting down")
	}

	rec = postJSON(t, handleRestart, running)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("restart: status = %d, body = %s, want 503", rec.Code, rec.Body.String())
	}
	if got := revision(); got != before {
		t.Errorf("restart bumped the revision from %d to %d", before, got)
	}

	rec, _ = postShutdown(t, f)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already shutting down") {
		t.Errorf("second shutdown: status = %d, body = %s, want 409", rec.Code, rec.Body.String())
	}
	if strings.Contains(f.called(), "stopLoops") || strings.Contains(f.called(), "exit") {
		t.Errorf("second shutdown called %s", f.called())
	}
}

func TestHandleShutdown_ReportsABalancerFailure(t *testing.T) {
	f := useShutdown(t)
	f.balancerOK = false
	f.balancer = errors.New("process 7 still running")

	rec, resp := postShutdown(t, f)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !slices.Contains(resp.Problems, "the load balancer: process 7 still running") {
		t.Errorf("Problems = %q, want the load balancer's", resp.Problems)
	}
	if resp.Balancer {
		t.Error("Balancer = true after a failure")
	}
	if want := "unit,stopLoops,stopBalancer,exit"; f.called() != want {
		t.Errorf("calls = %s, want %s: the daemon exits whatever went wrong", f.called(), want)
	}
}

func TestHandleShutdown_OnlyPOST(t *testing.T) {
	f := useShutdown(t)
	rec := httptest.NewRecorder()
	shutdownHandler(f.deps())(rec, httptest.NewRequest(http.MethodGet, "/shutdown", nil))
	if rec.Code != http.StatusMethodNotAllowed || len(f.calls) != 0 || shuttingDown.Load() {
		t.Errorf("GET: status = %d, calls = %s, shuttingDown = %v", rec.Code, f.called(), shuttingDown.Load())
	}
}
