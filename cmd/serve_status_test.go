/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/state"
)

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
	waitReady(t, running.Host, running.Port)

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

	if s := byName["running"]; !s.Alive || s.RSSBytes <= 0 {
		t.Errorf("running = %+v, want alive with a positive RSSBytes", s)
	}
	if s := byName["stopped"]; s.RSSBytes != 0 {
		t.Errorf("stopped = %+v, want RSSBytes 0", s)
	}
	if s := byName["reused"]; s.Alive || s.RSSBytes != 0 {
		t.Errorf("reused = %+v, want not alive and RSSBytes 0 (the stranger %d is not ours)", s, strangerPID)
	}
}
