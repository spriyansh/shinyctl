/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

func decodeApplyResponse(t *testing.T, rec *httptest.ResponseRecorder) api.ApplyResponse {
	t.Helper()
	var resp api.ApplyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return resp
}

func restartResponse(t *testing.T, spec manifest.AppSpec) api.ApplyResponse {
	t.Helper()
	rec := postJSON(t, handleRestart, spec)
	if rec.Code != http.StatusOK {
		t.Fatalf("restart %q: status = %d, body = %s", spec.AppName, rec.Code, rec.Body.String())
	}
	return decodeApplyResponse(t, rec)
}

// TestHandleRestart_RestartsRunningApp: restart replaces a running app's
// process with one started from the manifest as it is now, which a
// re-apply does not do when the port and name are unchanged.
func TestHandleRestart_RestartsRunningApp(t *testing.T) {
	cases := []struct {
		name    string
		adopted bool // first instance left behind by a previous daemon: no exit-watcher
		change  func(t *testing.T, old manifest.AppSpec, dir string, newPort int) manifest.AppSpec
	}{
		{
			name:   "unchanged manifest",
			change: func(t *testing.T, old manifest.AppSpec, dir string, p int) manifest.AppSpec { return old },
		},
		{
			name: "entrypoint-file and env change on the same port",
			change: func(t *testing.T, old manifest.AppSpec, dir string, p int) manifest.AppSpec {
				s := newTestSpec(t, dir, old.AppName, "other.R", old.Port)
				s.Env = map[string]string{"GREETING": "again"}
				return s
			},
		},
		{
			name: "port change",
			change: func(t *testing.T, old manifest.AppSpec, dir string, p int) manifest.AppSpec {
				old.Port = p
				return old
			},
		},
		{
			name:    "unchanged manifest on an app adopted after a shinyctl-api restart",
			adopted: true,
			change:  func(t *testing.T, old manifest.AppSpec, dir string, p int) manifest.AppSpec { return old },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useFakeRscript(t)
			dir := t.TempDir()
			ports := freePorts(t, 2)
			spec1 := newTestSpec(t, dir, "app", "app.R", ports[0])

			var a, b state.AppState
			var spec2 manifest.AppSpec
			t.Cleanup(func() {
				postJSON(t, handleDelete, spec1)
				killIfAlive(spec1, a)
				killIfAlive(spec2, b)
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

			spec2 = c.change(t, spec1, dir, ports[1])

			resp := restartResponse(t, spec2)
			if resp.Action != reconcile.ActionRestart {
				t.Errorf("action = %q, want %q", resp.Action, reconcile.ActionRestart)
			}
			if process.IsAlive(spec1, a) {
				t.Fatalf("previous instance (pid %d) is still running after restart", a.PID)
			}

			b = loadApp(t, spec2)
			if b.PID <= 0 || b.PID == a.PID {
				t.Fatalf("recorded pid = %d, want a new process (old %d)", b.PID, a.PID)
			}
			if b.EntrypointFile != spec2.EntrypointFile || b.Port != spec2.Port {
				t.Errorf("recorded %q on %d, want %q on %d", b.EntrypointFile, b.Port, spec2.EntrypointFile, spec2.Port)
			}
			if !process.IsAlive(spec2, b) {
				t.Errorf("new instance (pid %d) is not alive", b.PID)
			}
			waitReady(t, spec2.Host, spec2.Port)

			if want := fmt.Sprintf("pid %d", a.PID); !strings.Contains(resp.Message, want) {
				t.Errorf("message %q does not name %q", resp.Message, want)
			}
			if strings.Contains(resp.Message, "not signaled") {
				t.Errorf("message %q reports nothing was signaled", resp.Message)
			}

			applied, ok := appRegistry.Spec(spec2.AppStateFile)
			if !ok || applied.EntrypointFile != spec2.EntrypointFile || applied.Port != spec2.Port || applied.Env["GREETING"] != spec2.Env["GREETING"] {
				t.Errorf("registry holds %+v, want the restarted manifest %+v", applied, spec2)
			}

			if spec2.Port != spec1.Port {
				if err := process.PortAvailable(spec1.Host, spec1.Port); err != nil {
					t.Errorf("old port still held: %v", err)
				}
			}

			// The stopped process's exit must not be charged to its successor.
			assertNoExitRecorded(t, spec2, b.PID)
		})
	}
}

// TestHandleRestart_StartsAppThatIsNotRunning: restart is also "run this
// now" for an app that is stopped, was never applied, or is waiting out a
// crash loop or has given up, and it starts with a clean crash history.
func TestHandleRestart_StartsAppThatIsNotRunning(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, spec *manifest.AppSpec)
	}{
		{"stopped", func(t *testing.T, spec *manifest.AppSpec) {
			applyOK(t, *spec)
			if rec := postJSON(t, handleStop, *spec); rec.Code != http.StatusOK {
				t.Fatalf("stop: status = %d, body = %s", rec.Code, rec.Body.String())
			}
		}},
		{"never applied", func(t *testing.T, spec *manifest.AppSpec) {}},
		{"backing off", func(t *testing.T, spec *manifest.AppSpec) {
			if err := appRegistry.Remember(*spec); err != nil {
				t.Fatal(err)
			}
			saveExited(t, *spec, state.AppState{CrashCount: 5, LastExitAt: time.Now(), LastExitFailed: true})
		}},
		{"given up", func(t *testing.T, spec *manifest.AppSpec) {
			spec.RestartPolicy = manifest.RestartNever
			if err := appRegistry.Remember(*spec); err != nil {
				t.Fatal(err)
			}
			saveExited(t, *spec, state.AppState{LastExitAt: time.Now()})
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useFakeRscript(t)
			spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
			var b state.AppState
			t.Cleanup(func() {
				postJSON(t, handleDelete, spec)
				killIfAlive(spec, b)
			})

			c.setup(t, &spec)

			resp := restartResponse(t, spec)
			if resp.Action != reconcile.ActionRestart {
				t.Errorf("action = %q, want %q", resp.Action, reconcile.ActionRestart)
			}
			if !strings.HasPrefix(resp.Message, "Starting app") || strings.Contains(resp.Message, "not signaled") {
				t.Errorf("message = %q, want a plain start", resp.Message)
			}

			b = loadApp(t, spec)
			if b.PID <= 0 || !process.IsAlive(spec, b) {
				t.Fatalf("recorded app = %+v, want a running process", b)
			}
			waitReady(t, spec.Host, spec.Port)

			if b.CrashCount != 0 || !b.LastExitAt.IsZero() || b.LastExitFailed {
				t.Errorf("recorded app = %+v, want a clean crash history", b)
			}
			if !appRegistry.Managed(spec.AppStateFile) || appRegistry.IsStopped(spec.AppStateFile) {
				t.Error("app is not managed and running after restart")
			}
		})
	}
}

// saveExited records an app that is not running (PID 0) with the given
// exit history.
func saveExited(t *testing.T, spec manifest.AppSpec, app state.AppState) {
	t.Helper()
	app.Name = spec.AppName
	app.Port = spec.Port
	app.EntrypointFile = spec.EntrypointFile
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{app}}); err != nil {
		t.Fatal(err)
	}
}

// TestHandleRestart_LeavesReusedPIDAlone: the recorded app is dead and its
// PID now belongs to an unrelated process. Restart must not signal it, says
// so in the reply, and still starts the app.
func TestHandleRestart_LeavesReusedPIDAlone(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, b)
	})

	a, done, err := process.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
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

	resp := restartResponse(t, spec)
	select {
	case <-exited:
		t.Fatalf("pid %d was signaled, but it is not this app's process", pid)
	case <-time.After(200 * time.Millisecond):
	}
	if !strings.Contains(resp.Message, fmt.Sprintf("pid %d", pid)) || !strings.Contains(resp.Message, "not signaled") {
		t.Errorf("message %q does not say pid %d was not signaled", resp.Message, pid)
	}

	b = loadApp(t, spec)
	if b.PID <= 0 || b.PID == pid || !process.IsAlive(spec, b) {
		t.Fatalf("recorded app = %+v, want a new running process", b)
	}
	waitReady(t, spec.Host, spec.Port)
	assertNoExitRecorded(t, spec, b.PID)
}

// TestHandleRestart_DeadPIDGetsNoNote: the recorded app has exited and
// nothing took over its PID, which is the ordinary state of a crashed app.
func TestHandleRestart_DeadPIDGetsNoNote(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, b)
	})

	a, done, err := process.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	killIfAlive(spec, a)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake Rscript did not exit after SIGKILL")
	}
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{a}}); err != nil {
		t.Fatal(err)
	}
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}

	resp := restartResponse(t, spec)
	if strings.Contains(resp.Message, "not signaled") || strings.Contains(resp.Message, "stopped the previous instance") {
		t.Errorf("message %q mentions a process that was simply gone", resp.Message)
	}

	b = loadApp(t, spec)
	if b.PID <= 0 || !process.IsAlive(spec, b) {
		t.Fatalf("recorded app = %+v, want a running process", b)
	}
	waitReady(t, spec.Host, spec.Port)
}

// TestHandleRestart_OntoTakenPort pins the cost of stopping the old
// instance before the port check, as apply's replacement path does: a
// restart onto a port someone else holds fails, and the app is left down
// with exactly one crash recorded and no stale PID.
func TestHandleRestart_OntoTakenPort(t *testing.T) {
	hold := func(t *testing.T, spec manifest.AppSpec) {
		ln, err := net.Listen("tcp4", net.JoinHostPort(spec.Host, strconv.Itoa(spec.Port)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
	}

	t.Run("running app restarted onto a taken port", func(t *testing.T) {
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

		spec2 := spec1
		spec2.Port = ports[1]
		hold(t, spec2)

		if rec := postJSON(t, handleRestart, spec2); rec.Code != http.StatusInternalServerError {
			t.Fatalf("restart onto a taken port: status = %d, want 500", rec.Code)
		}
		if process.IsAlive(spec1, a) {
			t.Error("previous instance still running: it is stopped before the port check")
		}

		time.Sleep(250 * time.Millisecond)
		app := loadApp(t, spec1)
		if app.PID != 0 || app.CrashCount != 1 || !app.LastExitFailed {
			t.Errorf("recorded app = %+v, want pid 0 and exactly one failed exit", app)
		}
	})

	t.Run("crash-looping app whose port is now taken", func(t *testing.T) {
		useFakeRscript(t)
		spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
		t.Cleanup(func() { postJSON(t, handleDelete, spec) })

		if err := appRegistry.Remember(spec); err != nil {
			t.Fatal(err)
		}
		saveExited(t, spec, state.AppState{CrashCount: 4, LastExitAt: time.Now(), LastExitFailed: true})
		hold(t, spec)

		if rec := postJSON(t, handleRestart, spec); rec.Code != http.StatusInternalServerError {
			t.Fatalf("restart onto a taken port: status = %d, want 500", rec.Code)
		}

		time.Sleep(250 * time.Millisecond)
		app := loadApp(t, spec)
		if app.PID != 0 || app.CrashCount != 1 || !app.LastExitFailed {
			t.Errorf("recorded app = %+v, want pid 0 and one crash (the history starts over)", app)
		}
	})
}

func TestHandleRestart_RejectsInvalidManifest(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	t.Cleanup(func() { appRegistry.Forget(spec.AppStateFile) })
	if err := os.Remove(spec.EntrypointFile); err != nil {
		t.Fatal(err)
	}

	if rec := postJSON(t, handleRestart, spec); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body = %s)", rec.Code, rec.Body.String())
	}
	if _, ok := appRegistry.Spec(spec.AppStateFile); ok {
		t.Error("registry remembered a manifest that failed validation")
	}
}
