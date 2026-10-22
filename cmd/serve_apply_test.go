/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

// TestHandleApply_ReplacesPreviousInstance: a re-apply that changes the
// port or the app name starts a new process, and the recorded one must be
// stopped first. Otherwise it keeps serving while state.json forgets it,
// so status can't see it and delete can't stop it.
func TestHandleApply_ReplacesPreviousInstance(t *testing.T) {
	cases := []struct {
		name     string
		adopted  bool // first instance left behind by a previous daemon: no exit-watcher
		change   func(t *testing.T, old manifest.AppSpec, dir string, newPort int) manifest.AppSpec
		samePort bool
	}{
		{
			name: "port change",
			change: func(t *testing.T, old manifest.AppSpec, dir string, p int) manifest.AppSpec {
				old.Port = p
				return old
			},
		},
		{
			name:     "app-name change on the same port",
			samePort: true,
			change: func(t *testing.T, old manifest.AppSpec, dir string, p int) manifest.AppSpec {
				old.AppName = "renamed"
				return old
			},
		},
		{
			name: "entrypoint-file and port change",
			change: func(t *testing.T, old manifest.AppSpec, dir string, p int) manifest.AppSpec {
				return newTestSpec(t, dir, old.AppName, "other.R", p)
			},
		},
		{
			name:    "port change on an app adopted after a shinyctl-api restart",
			adopted: true,
			change: func(t *testing.T, old manifest.AppSpec, dir string, p int) manifest.AppSpec {
				old.Port = p
				return old
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useFakeRscript(t)
			dir := t.TempDir()
			ports := freePorts(t, 2)
			spec1 := newTestSpec(t, dir, "app", "app.R", ports[0])

			// Registered before anything starts, so a failure while setting
			// up can't leave the first instance running and managed. Delete
			// stops whatever state.json records (spec1 and spec2 share the
			// state file); the kill is identity-checked and a no-op for a
			// PID that was never recorded.
			var a state.AppState
			t.Cleanup(func() {
				postJSON(t, handleDelete, spec1)
				killIfAlive(spec1, a)
			})
			if c.adopted {
				if err := appRegistry.Remember(spec1); err != nil {
					t.Fatal(err)
				}
				var err error
				a, _, err = process.Start(spec1)
				if err != nil {
					t.Fatal(err)
				}
				if err := state.Save(spec1.AppStateFile, state.State{AppGroup: spec1.AppGroup, Apps: []state.AppState{a}}); err != nil {
					t.Fatal(err)
				}
			} else {
				a = applyOK(t, spec1)
			}
			waitReady(t, spec1.Host, spec1.Port)

			spec2 := c.change(t, spec1, dir, ports[1])

			rec := postJSON(t, handleApply, spec2)
			if rec.Code != http.StatusOK {
				t.Fatalf("apply: status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if process.IsAlive(spec1, a) {
				t.Fatalf("previous instance (pid %d) is still running after re-apply", a.PID)
			}

			b := loadApp(t, spec2)
			if b.PID <= 0 || b.PID == a.PID {
				t.Fatalf("recorded pid = %d, want a new process (old %d)", b.PID, a.PID)
			}
			if b.Name != spec2.AppName || b.Port != spec2.Port {
				t.Errorf("recorded %q on %d, want %q on %d", b.Name, b.Port, spec2.AppName, spec2.Port)
			}
			if !process.IsAlive(spec2, b) {
				t.Errorf("new instance (pid %d) is not alive", b.PID)
			}
			waitReady(t, spec2.Host, spec2.Port)
			if !c.samePort {
				if err := process.PortAvailable(spec1.Host, spec1.Port); err != nil {
					t.Errorf("old port still held: %v", err)
				}
			}

			// The stopped process's exit must not be charged to its successor.
			assertNoExitRecorded(t, spec2, b.PID)

			if rec := postJSON(t, handleDelete, spec2); rec.Code != http.StatusOK {
				t.Fatalf("delete: status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if process.IsAlive(spec2, b) {
				t.Error("new instance survived delete")
			}
			if process.IsReady(spec2.Host, spec2.Port) {
				t.Error("port still serving after delete")
			}
		})
	}
}

// TestHandleApply_ReplacementOntoTakenPort pins the cost of stopping the
// old instance before the port check: a re-apply onto a port someone else
// holds fails, and the app is left down with exactly one crash recorded and
// no stale PID.
func TestHandleApply_ReplacementOntoTakenPort(t *testing.T) {
	useFakeRscript(t)
	dir := t.TempDir()
	ports := freePorts(t, 2)
	spec1 := newTestSpec(t, dir, "app", "app.R", ports[0])

	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec1)
		killIfAlive(spec1, a)
	})
	a = applyOK(t, spec1)
	waitReady(t, spec1.Host, spec1.Port)

	ln, err := net.Listen("tcp4", net.JoinHostPort(spec1.Host, strconv.Itoa(ports[1])))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	spec2 := spec1
	spec2.Port = ports[1]
	if rec := postJSON(t, handleApply, spec2); rec.Code != http.StatusInternalServerError {
		t.Fatalf("apply onto a taken port: status = %d, want 500", rec.Code)
	}
	if process.IsAlive(spec1, a) {
		t.Error("previous instance still running: it is stopped before the port check")
	}

	time.Sleep(250 * time.Millisecond)
	app := loadApp(t, spec1)
	if app.PID != 0 || app.CrashCount != 1 || !app.LastExitFailed {
		t.Errorf("recorded app = %+v, want pid 0 and exactly one failed exit", app)
	}
}

// TestHandleApply_RestartsOnManifestChange: a re-apply that changes a field
// shaping the process replaces the running one, even when the port and name
// stay the same.
func TestHandleApply_RestartsOnManifestChange(t *testing.T) {
	cases := []struct {
		field  string
		change func(t *testing.T, old manifest.AppSpec, dir string) manifest.AppSpec
	}{
		{"env", func(t *testing.T, old manifest.AppSpec, dir string) manifest.AppSpec {
			old.Env = map[string]string{"GREETING": "again"}
			return old
		}},
		{"host", func(t *testing.T, old manifest.AppSpec, dir string) manifest.AppSpec {
			old.Host = "0.0.0.0"
			return old
		}},
		{"working-dir", func(t *testing.T, old manifest.AppSpec, dir string) manifest.AppSpec {
			old.WorkingDir = t.TempDir()
			return old
		}},
		{"entrypoint-file", func(t *testing.T, old manifest.AppSpec, dir string) manifest.AppSpec {
			return newTestSpec(t, dir, old.AppName, "other.R", old.Port)
		}},
		{"app-group", func(t *testing.T, old manifest.AppSpec, dir string) manifest.AppSpec {
			old.AppGroup = "other-group"
			return old
		}},
	}

	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			useFakeRscript(t)
			dir := t.TempDir()
			spec1 := newTestSpec(t, dir, "app", "app.R", freePorts(t, 1)[0])

			var a, b state.AppState
			spec2 := spec1
			t.Cleanup(func() {
				postJSON(t, handleDelete, spec1)
				killIfAlive(spec1, a)
				killIfAlive(spec2, b)
			})
			a = applyOK(t, spec1)
			waitReady(t, spec1.Host, spec1.Port)

			spec2 = c.change(t, spec1, dir)
			rec := postJSON(t, handleApply, spec2)
			if rec.Code != http.StatusOK {
				t.Fatalf("apply: status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if process.IsAlive(spec1, a) {
				t.Fatalf("previous instance (pid %d) is still running after the apply", a.PID)
			}

			b = loadApp(t, spec2)
			if b.PID <= 0 || b.PID == a.PID {
				t.Fatalf("recorded pid = %d, want a new process (old %d)", b.PID, a.PID)
			}
			if !reflect.DeepEqual(b.StartedFrom, spec2.StartFields()) {
				t.Errorf("StartedFrom = %v, want %v", b.StartedFrom, spec2.StartFields())
			}
			msg := responseMessage(t, rec)
			if want := "manifest changed: " + c.field + ";"; !strings.Contains(msg, want) {
				t.Errorf("message %q does not contain %q", msg, want)
			}
			if want := fmt.Sprintf("stopped the previous instance: pid %d", a.PID); !strings.Contains(msg, want) {
				t.Errorf("message %q does not contain %q", msg, want)
			}
			waitReady(t, spec2.EffectiveHost(), spec2.Port)
			assertNoExitRecorded(t, spec2, b.PID)
		})
	}
}

// TestHandleApply_UnchangedManifestKeepsProcess: applying the same manifest
// again, or the copy of it the registry restores from JSON, changes nothing.
func TestHandleApply_UnchangedManifestKeepsProcess(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	spec.Env = map[string]string{"GREETING": "hi"}
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
	})
	a = applyOK(t, spec)
	waitReady(t, spec.Host, spec.Port)

	var roundTripped manifest.AppSpec
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatal(err)
	}

	for name, s := range map[string]manifest.AppSpec{"same spec": spec, "round-tripped spec": roundTripped} {
		rec := postJSON(t, handleApply, s)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", name, rec.Code, rec.Body.String())
		}
		if msg := responseMessage(t, rec); msg != "Nothing to change" {
			t.Errorf("%s: message = %q, want %q", name, msg, "Nothing to change")
		}
		if got := loadApp(t, spec); got.PID != a.PID {
			t.Errorf("%s: pid = %d, want the unchanged %d", name, got.PID, a.PID)
		}
	}
}

func TestHandleApply_RestartPolicyChangeNeedsNoRestart(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
	})
	a = applyOK(t, spec)
	waitReady(t, spec.Host, spec.Port)

	spec2 := spec
	spec2.RestartPolicy = manifest.RestartNever
	rec := postJSON(t, handleApply, spec2)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	want := `Updated "app" without a restart (manifest changed: restart-policy)`
	if msg := responseMessage(t, rec); msg != want {
		t.Errorf("message = %q, want %q", msg, want)
	}
	if got := loadApp(t, spec); got.PID != a.PID || !process.IsAlive(spec, got) {
		t.Errorf("recorded app = %+v, want the original process %d still running", got, a.PID)
	}
	if applied, _ := appRegistry.Spec(spec.AppStateFile); applied.RestartPolicy != manifest.RestartNever {
		t.Errorf("registry restart-policy = %q, want never", applied.RestartPolicy)
	}
}

// TestHandleApply_OldRecordIsNotRestarted: a process recorded before
// started-from existed (the state a shinyctl upgrade finds) is not restarted
// by a manifest change it has no way to compare against.
func TestHandleApply_OldRecordIsNotRestarted(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
	})
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	var err error
	a, _, err = process.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	a.StartedFrom = nil
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{a}}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, spec.Host, spec.Port)

	spec2 := spec
	spec2.Env = map[string]string{"GREETING": "again"}
	rec := postJSON(t, handleApply, spec2)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if msg := responseMessage(t, rec); msg != "Nothing to change" {
		t.Errorf("message = %q, want %q", msg, "Nothing to change")
	}
	if got := loadApp(t, spec); got.PID != a.PID {
		t.Errorf("pid = %d, want the unchanged %d", got.PID, a.PID)
	}
}

// TestTriggerReconcile_ConvergesToRegistrySpec: the ticker brings a running
// process in line with the registry, so an apply that updated the registry
// but could not finish the replacement is completed on the next tick.
func TestTriggerReconcile_ConvergesToRegistrySpec(t *testing.T) {
	useFakeRscript(t)
	spec1 := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var a, b state.AppState
	spec2 := spec1
	spec2.Env = map[string]string{"GREETING": "again"}
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec1)
		killIfAlive(spec1, a)
		killIfAlive(spec2, b)
	})
	a = applyOK(t, spec1)
	waitReady(t, spec1.Host, spec1.Port)

	if err := appRegistry.Remember(spec2); err != nil {
		t.Fatal(err)
	}
	triggerReconcile(spec1.AppStateFile)

	if process.IsAlive(spec1, a) {
		t.Fatalf("previous instance (pid %d) is still running after the tick", a.PID)
	}
	b = loadApp(t, spec2)
	if b.PID <= 0 || b.PID == a.PID || !reflect.DeepEqual(b.StartedFrom, spec2.StartFields()) {
		t.Fatalf("recorded app = %+v, want a new process started from spec2", b)
	}
	waitReady(t, spec2.Host, spec2.Port)
	assertNoExitRecorded(t, spec2, b.PID)
}

// TestTriggerReconcile_LeavesOldRecordsAlone: after a shinyctl upgrade the
// first ticks must not restart apps whose records predate started-from.
func TestTriggerReconcile_LeavesOldRecordsAlone(t *testing.T) {
	useFakeRscript(t)
	spec1 := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec1)
		killIfAlive(spec1, a)
	})
	if err := appRegistry.Remember(spec1); err != nil {
		t.Fatal(err)
	}
	var err error
	a, _, err = process.Start(spec1)
	if err != nil {
		t.Fatal(err)
	}
	a.StartedFrom = nil
	if err := state.Save(spec1.AppStateFile, state.State{AppGroup: spec1.AppGroup, Apps: []state.AppState{a}}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, spec1.Host, spec1.Port)

	// An edit of the entrypoint too: older daemons recorded it but never
	// acted on its change.
	spec2 := newTestSpec(t, filepath.Dir(spec1.EntrypointFile), "app", "other.R", spec1.Port)
	spec2.Env = map[string]string{"GREETING": "again"}
	spec2.Host = "0.0.0.0"
	if err := appRegistry.Remember(spec2); err != nil {
		t.Fatal(err)
	}
	triggerReconcile(spec1.AppStateFile)

	if got := loadApp(t, spec1); got.PID != a.PID || !process.IsAlive(spec1, got) {
		t.Errorf("recorded app = %+v, want the original process %d still running", got, a.PID)
	}
}
