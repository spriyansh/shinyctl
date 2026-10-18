/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

func postJSON(t *testing.T, handler http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(data)))
	return rec
}

// TestHandleDelete covers the forget half of delete; the stop half is
// process.Stop's own tests. The app here has no live process (PID 0), so
// what's under test is: registry entry gone, stopped flag gone, state
// file gone, and the exit-watcher/ticker gate (Managed) closed.
func TestHandleDelete(t *testing.T) {
	dir := t.TempDir()
	spec := manifest.AppSpec{
		AppGroup:     "g",
		AppName:      "n",
		AppStateFile: filepath.Join(dir, "state.json"),
	}
	t.Cleanup(func() { appRegistry.Forget(spec.AppStateFile) })

	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: "g", Apps: []state.AppState{{Name: "n"}}}); err != nil {
		t.Fatal(err)
	}

	rec := postJSON(t, handleDelete, spec)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	if _, err := os.Stat(spec.AppStateFile); !os.IsNotExist(err) {
		t.Errorf("state file still exists after delete (stat err = %v)", err)
	}
	for _, remembered := range appRegistry.All() {
		if remembered.AppStateFile == spec.AppStateFile {
			t.Error("registry still lists the app after delete")
		}
	}
	if appRegistry.IsStopped(spec.AppStateFile) {
		t.Error("stopped flag survived delete")
	}
	if appRegistry.Managed(spec.AppStateFile) {
		t.Error("Managed = true after delete — the exit-watcher would restart it")
	}

	// Idempotent: deleting again, with nothing to delete, is still OK.
	if rec := postJSON(t, handleDelete, spec); rec.Code != http.StatusOK {
		t.Errorf("second delete: status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleDelete_RequiresNameAndStateFile(t *testing.T) {
	for _, spec := range []manifest.AppSpec{
		{AppName: "n"},
		{AppStateFile: "/x/state.json"},
	} {
		if rec := postJSON(t, handleDelete, spec); rec.Code != http.StatusBadRequest {
			t.Errorf("spec %+v: status = %d, want 400", spec, rec.Code)
		}
	}
}
