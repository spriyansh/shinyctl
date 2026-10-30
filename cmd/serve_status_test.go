/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

// statusOf asks the status handler about the app called name.
func statusOf(t *testing.T, name string) api.AppStatus {
	t.Helper()
	rec := httptest.NewRecorder()
	handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var resp api.StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, s := range resp.Apps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no app %q in the status", name)
	return api.AppStatus{}
}

// TestHandleStatus_ReportsMemory: a running app shows its memory, a stopped
// one doesn't, and a record whose PID now belongs to a stranger never shows
// the stranger's memory.
func TestHandleStatus_ReportsMemory(t *testing.T) {
	useFakeRscript(t)
	ports := freePorts(t, 3)

	running := newTestSpec(t, t.TempDir(), "running", "app.R", ports[0])
	stopped := newTestSpec(t, t.TempDir(), "stopped", "app.R", ports[1])
	reused := newTestSpec(t, t.TempDir(), "reused", "app.R", ports[2])

	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, running)
		postJSON(t, handleDelete, stopped)
		postJSON(t, handleDelete, reused)
		killIfAlive(running, a)
	})

	a = applyOK(t, running)
	waitServed(t, running, a.PID)

	applyOK(t, stopped)
	if rec := postJSON(t, handleStop, stopped); rec.Code != http.StatusOK {
		t.Fatalf("stop: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// A dead app whose recorded PID is now someone else's process.
	if err := appRegistry.Remember(reused); err != nil {
		t.Fatal(err)
	}
	strangerPID, _ := startStranger(t)
	if err := state.Save(reused.AppStateFile, state.State{AppGroup: reused.AppGroup, Apps: []state.AppState{{
		Name: reused.AppName, PID: strangerPID, Port: reused.Port, EntrypointFile: reused.EntrypointFile, StartedAt: a.StartedAt,
	}}}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp api.StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	byName := map[string]api.AppStatus{}
	for _, s := range resp.Apps {
		byName[s.Name] = s
	}

	if s := byName["running"]; len(s.Copies) != 1 || !s.Copies[0].Alive || s.Copies[0].MemoryBytes <= 0 {
		t.Errorf("running = %+v, want one alive copy with a positive MemoryBytes", s)
	}
	if s := byName["stopped"]; len(s.Copies) != 0 {
		t.Errorf("stopped = %+v, want no copies", s)
	}
	if s := byName["reused"]; len(s.Copies) != 1 || s.Copies[0].Alive || s.Copies[0].MemoryBytes != 0 {
		t.Errorf("reused = %+v, want one copy, not alive and MemoryBytes 0 (the stranger %d is not ours)", s, strangerPID)
	}
	if resp.BalancerMemory <= 0 {
		t.Errorf("BalancerMemory = %d, want the load balancer's memory", resp.BalancerMemory)
	}
}

// TestHandleStatus_Ready: an app is ready once its process answers and the
// load balancer serves its port. A process the load balancer was never told
// about is alive but not ready.
func TestHandleStatus_Ready(t *testing.T) {
	useFakeRscript(t)
	ports := freePorts(t, 2)
	running := newTestSpec(t, t.TempDir(), "running", "app.R", ports[0])
	unsynced := newTestSpec(t, t.TempDir(), "unsynced", "app.R", ports[1])

	var a, b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, running)
		postJSON(t, handleDelete, unsynced)
		killIfAlive(running, a)
		killIfAlive(unsynced, b)
	})
	a = applyOK(t, running)
	waitServed(t, running, a.PID)

	// Started and recorded, but not synced into the load balancer.
	if err := appRegistry.Remember(unsynced); err != nil {
		t.Fatal(err)
	}
	b, _ = startRaw(t, unsynced, 1)
	if err := state.Save(unsynced.AppStateFile, state.State{AppGroup: unsynced.AppGroup, Apps: []state.AppState{b}}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, process.InternalHost, b.Port)

	status := func() map[string]api.AppStatus {
		rec := httptest.NewRecorder()
		handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
		var resp api.StatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		byName := map[string]api.AppStatus{}
		for _, s := range resp.Apps {
			byName[s.Name] = s
		}
		return byName
	}

	got := status()
	if s := got["running"]; len(s.Copies) != 1 || !s.Copies[0].Alive || !s.Copies[0].Ready || s.Port != running.Port {
		t.Errorf("running = %+v, want an alive, ready copy on the app's own port", s)
	}
	if s := got["unsynced"]; len(s.Copies) != 1 || !s.Copies[0].Alive || s.Copies[0].Ready {
		t.Errorf("unsynced = %+v, want an alive copy that is not ready", s)
	}

	refreshPools()
	if err := syncProxy(""); err != nil {
		t.Fatal(err)
	}
	if s := status()["unsynced"]; len(s.Copies) != 1 || !s.Copies[0].Ready || s.Copies[0].Role != "active" {
		t.Errorf("unsynced after the sync = %+v, want an active, ready copy", s)
	}
}

func newStatusRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }

func newStatusRequest() *http.Request { return httptest.NewRequest(http.MethodGet, "/status", nil) }

func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) api.StatusResponse {
	t.Helper()
	var resp api.StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return resp
}

// TestHandleStatus_AppsInAFixedOrder: by name, then group, then port, however
// many times it is asked; the registry itself keeps its apps in a map. The
// ports are chosen so that the group decides between two apps of one name.
func TestHandleStatus_AppsInAFixedOrder(t *testing.T) {
	type app struct {
		name, group string
		port        int
	}
	apps := []app{{"ord-d", "x", 18000}, {"ord-b", "x", 18001}, {"ord-a", "y", 18002}, {"ord-c", "b", 18003}, {"ord-a", "x", 18004}, {"ord-c", "a", 18005}}
	for _, a := range apps {
		spec := newTestSpec(t, t.TempDir(), a.name, "app.R", a.port)
		spec.AppGroup = a.group
		if err := appRegistry.Remember(spec); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { appRegistry.Forget(spec.AppStateFile) })
	}
	want := []int{18004, 18002, 18001, 18005, 18003, 18000}

	for round := 0; round < 25; round++ {
		rec := httptest.NewRecorder()
		handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
		var resp api.StatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, s := range resp.Apps {
			if strings.HasPrefix(s.Name, "ord-") {
				got = append(got, s.Port)
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("round %d: ports in order %v, want %v", round, got, want)
		}
	}
}
