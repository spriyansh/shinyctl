/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"io"
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

// TestHandleRestart_RestartsRunningApp: restart starts a new copy from the
// manifest as it is now, which a re-apply does not do when the port and name
// are unchanged; the old copy drains and stops.
func TestHandleRestart_RestartsRunningApp(t *testing.T) {
	cases := []struct {
		name    string
		adopted bool   // first instance left behind by a previous daemon: no exit-watcher
		changes string // the fields the reply says changed
		change  func(t *testing.T, old manifest.AppSpec, dir string, newPort int) manifest.AppSpec
	}{
		{
			name:   "unchanged manifest",
			change: func(t *testing.T, old manifest.AppSpec, dir string, p int) manifest.AppSpec { return old },
		},
		{
			name:    "entrypoint-file and env change on the same port",
			changes: "entrypoint-file, env",
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
			settleNow(t)
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
				a = startCopy(t, spec1)
			} else {
				a = applyOK(t, spec1)
			}
			waitServed(t, spec1, a.PID)

			spec2 = c.change(t, spec1, dir, ports[1])

			resp := restartResponse(t, spec2)
			if resp.Action != reconcile.ActionRestart {
				t.Errorf("action = %q, want %q", resp.Action, reconcile.ActionRestart)
			}
			changed := ""
			if c.changes != "" {
				changed = " (manifest changed: " + c.changes + ")"
			}
			if want := fmt.Sprintf("Restarting app %q%s: starting copy 2; copy 1 keeps serving until the new one is ready, then drains for up to 15m", spec2.AppName, changed); resp.Message != want {
				t.Errorf("message = %q, want %q", resp.Message, want)
			}

			// The old copy serves until the new one is ready, then drains and,
			// with no visitors, stops.
			waitStopped(t, spec1, a)
			b = newestApp(t, spec2)
			if b.PID <= 0 || b.PID == a.PID || b.Copy == a.Copy {
				t.Fatalf("recorded app = %+v, want a new copy (old %d)", b, a.PID)
			}
			if b.EntrypointFile != spec2.EntrypointFile {
				t.Errorf("recorded %q, want %q", b.EntrypointFile, spec2.EntrypointFile)
			}
			if !process.IsAlive(spec2, b) {
				t.Errorf("new instance (pid %d) is not alive", b.PID)
			}
			waitServed(t, spec2, b.PID)

			applied, ok := appRegistry.Spec(spec2.AppStateFile)
			if !ok || applied.EntrypointFile != spec2.EntrypointFile || applied.Port != spec2.Port || applied.Env["GREETING"] != spec2.Env["GREETING"] {
				t.Errorf("registry holds %+v, want the restarted manifest %+v", applied, spec2)
			}

			if spec2.Port != spec1.Port {
				waitClosed(t, spec1.Host, spec1.Port)
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
			waitServed(t, spec, b.PID)

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
	waitServed(t, spec, b.PID)
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

	a, done := startRaw(t, spec, 1)
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
	waitServed(t, spec, b.PID)
}

// TestHandleRestart_OntoTakenPort: a restart onto a port the load balancer
// can't take is refused before the app is stopped, so it keeps serving where
// it was. An app that is not being served has no listener to protect: the
// start fails, and since nothing ran it is reported as a port in use, not
// counted as a crash.
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
		waitServed(t, spec1, a.PID)

		spec2 := spec1
		spec2.Port = ports[1]
		hold(t, spec2)

		rec := postJSON(t, handleRestart, spec2)
		if rec.Code != http.StatusConflict {
			t.Fatalf("restart onto a taken port: status = %d, want 409 (body = %s)", rec.Code, rec.Body.String())
		}
		if !process.IsAlive(spec1, a) {
			t.Error("previous instance was stopped although the move was refused")
		}
		if got := loadApp(t, spec1); got.PID != a.PID || got.CrashCount != 0 {
			t.Errorf("recorded app = %+v, want the original process %d and no crash", got, a.PID)
		}
		waitServed(t, spec1, a.PID)
		if applied, _ := appRegistry.Spec(spec1.AppStateFile); applied.Port != spec1.Port {
			t.Errorf("registry port = %d, want the old %d", applied.Port, spec1.Port)
		}
	})

	t.Run("crash-looping app whose port is now taken is port-in-use", func(t *testing.T) {
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
		if app.PID != 0 || app.CrashCount != 0 || !app.LastExitAt.IsZero() {
			t.Errorf("recorded app = %+v, want pid 0 and no crash (the restart cleared the history, and the port is not the app's fault)", app)
		}
		if s := statusOf(t, "app"); !strings.Contains(s.NotStarted, "is not available") {
			t.Errorf("status = %+v, want NotStarted to say the port is taken", s)
		}
	})
}

// TestHandleRestart_KeepsInternalPort: a restart reuses the copy's internal
// port, so the load balancer needs no reload. The record names a port far
// from the lowest free one, which a fresh pick would choose.
func TestHandleRestart_KeepsInternalPort(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, b)
	})
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{{
		Name: spec.AppName, Copy: 1, Port: 29990, EntrypointFile: spec.EntrypointFile, LastExitAt: time.Now(), LastExitFailed: true,
	}}}); err != nil {
		t.Fatal(err)
	}

	restartResponse(t, spec)
	b = loadApp(t, spec)
	if b.Copy != 1 || b.Port != 29990 {
		t.Errorf("recorded app = %+v, want the internal port 29990 kept", b)
	}
	waitServed(t, spec, b.PID)
}

// TestHandleRestart_RequestsWaitOutTheRestart: requests made to the app's
// port while it restarts all succeed, served by the old process and then the
// new one.
func TestHandleRestart_RequestsWaitOutTheRestart(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var a, b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
		killIfAlive(spec, b)
	})
	a = applyOK(t, spec)
	waitServed(t, spec, a.PID)

	stop := make(chan struct{})
	seen := make(chan map[string]int, 1)
	failed := make(chan string, 1)
	go func() {
		counts := map[string]int{}
		client := &http.Client{Timeout: 20 * time.Second}
		url := "http://" + dialAddr(spec.Host, spec.Port) + "/"
		for {
			select {
			case <-stop:
				seen <- counts
				return
			default:
			}
			resp, err := client.Get(url)
			if err != nil {
				select {
				case failed <- err.Error():
				default:
				}
			} else {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					select {
					case failed <- fmt.Sprintf("status %d: %s", resp.StatusCode, body):
					default:
					}
				}
				counts[string(body)]++
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	time.Sleep(150 * time.Millisecond) // some requests reach the old process first
	restartResponse(t, spec)
	b = loadApp(t, spec)
	waitServed(t, spec, b.PID)
	close(stop)

	select {
	case msg := <-failed:
		t.Errorf("a request during the restart failed: %s", msg)
	default:
	}
	counts := <-seen
	for _, pid := range []int{a.PID, b.PID} {
		if counts[fmt.Sprintf("pid=%d", pid)] == 0 {
			t.Errorf("no request was served by pid %d: %v", pid, counts)
		}
	}
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
