/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

func responseMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp api.ApplyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return resp.Message
}

// startStranger runs a process that has nothing to do with the app under
// test, killed by handle on cleanup (reuse-safe). exited closes when it is
// gone.
func startStranger(t *testing.T) (pid int, exited <-chan struct{}) {
	t.Helper()
	s := exec.Command("sleep", "30")
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	ch := make(chan struct{})
	go func() {
		s.Wait()
		close(ch)
	}()
	t.Cleanup(func() {
		s.Process.Kill()
		<-ch
	})
	return s.Process.Pid, ch
}

type stopHandler struct {
	name    string
	handler http.HandlerFunc
}

var stopHandlers = []stopHandler{{"stop", handleStop}, {"delete", handleDelete}}

// TestStopAndDelete_LeaveReusedPIDAlone: the recorded app is dead and its
// PID now belongs to an unrelated process. stop and delete must not signal
// it, but still do their bookkeeping.
func TestStopAndDelete_LeaveReusedPIDAlone(t *testing.T) {
	for _, h := range stopHandlers {
		t.Run(h.name, func(t *testing.T) {
			useFakeRscript(t)
			spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
			t.Cleanup(func() { appRegistry.Forget(spec.AppStateFile) })

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

			rec := postJSON(t, h.handler, spec)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			select {
			case <-exited:
				t.Fatalf("pid %d was signaled, but it is not this app's process", pid)
			case <-time.After(200 * time.Millisecond):
			}
			if msg := responseMessage(t, rec); !strings.Contains(msg, fmt.Sprintf("pid %d", pid)) {
				t.Errorf("message %q does not name pid %d", msg, pid)
			}

			if h.name == "stop" {
				if !appRegistry.IsStopped(spec.AppStateFile) {
					t.Error("app not marked stopped")
				}
				if got := loadApp(t, spec); got.PID != 0 {
					t.Errorf("recorded pid = %d, want 0", got.PID)
				}
			} else {
				if appRegistry.Managed(spec.AppStateFile) {
					t.Error("app still managed after delete")
				}
				if _, err := os.Stat(spec.AppStateFile); !os.IsNotExist(err) {
					t.Errorf("state file still exists (err = %v)", err)
				}
			}
		})
	}
}

// TestStopAndDelete_StopOurProcess guards that the identity check doesn't
// block legitimate stops, for apps this daemon started and for adopted ones.
func TestStopAndDelete_StopOurProcess(t *testing.T) {
	for _, h := range stopHandlers {
		for _, adopted := range []bool{false, true} {
			name := h.name + "/applied"
			if adopted {
				name = h.name + "/adopted"
			}
			t.Run(name, func(t *testing.T) {
				useFakeRscript(t)
				spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
				var a state.AppState
				t.Cleanup(func() {
					postJSON(t, handleDelete, spec)
					killIfAlive(spec, a)
				})

				if adopted {
					if err := appRegistry.Remember(spec); err != nil {
						t.Fatal(err)
					}
					var err error
					a, _, err = process.Start(spec)
					if err != nil {
						t.Fatal(err)
					}
					if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{a}}); err != nil {
						t.Fatal(err)
					}
				} else {
					a = applyOK(t, spec)
				}
				waitReady(t, spec.Host, spec.Port)

				rec := postJSON(t, h.handler, spec)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
				}
				if process.IsAlive(spec, a) {
					t.Error("process still running")
				}
				if process.IsReady(spec.Host, spec.Port) {
					t.Error("port still serving")
				}
				if msg := responseMessage(t, rec); strings.Contains(msg, "not signaled") {
					t.Errorf("message %q reports nothing was signaled", msg)
				}
				if h.name == "stop" {
					if !appRegistry.IsStopped(spec.AppStateFile) || loadApp(t, spec).PID != 0 {
						t.Error("stop did not leave the app stopped with a cleared record")
					}
				} else {
					if appRegistry.Managed(spec.AppStateFile) {
						t.Error("app still managed after delete")
					}
					if _, err := os.Stat(spec.AppStateFile); !os.IsNotExist(err) {
						t.Errorf("state file still exists (err = %v)", err)
					}
				}
			})
		}
	}
}

// TestStopAndDelete_OldFormatRecord: records written before entrypoint-file
// was recorded are matched against the request's entrypoint or the one the
// registry last applied.
func TestStopAndDelete_OldFormatRecord(t *testing.T) {
	setup := func(t *testing.T, remember bool) (manifest.AppSpec, state.AppState) {
		useFakeRscript(t)
		dir := t.TempDir()
		spec := newTestSpec(t, dir, "app", "app.R", freePorts(t, 1)[0])
		var a state.AppState
		t.Cleanup(func() {
			appRegistry.Forget(spec.AppStateFile)
			killIfAlive(spec, a)
		})
		if remember {
			if err := appRegistry.Remember(spec); err != nil {
				t.Fatal(err)
			}
		}
		var err error
		a, _, err = process.Start(spec)
		if err != nil {
			t.Fatal(err)
		}
		a.EntrypointFile = ""
		if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{a}}); err != nil {
			t.Fatal(err)
		}
		waitReady(t, spec.Host, spec.Port)
		return spec, a
	}

	t.Run("stop with an edited manifest", func(t *testing.T) {
		spec, a := setup(t, true)
		edited := newTestSpec(t, spec.WorkingDir, spec.AppName, "other.R", spec.Port)
		if rec := postJSON(t, handleStop, edited); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if process.IsAlive(spec, a) {
			t.Error("process still running")
		}
	})

	t.Run("delete with no entrypoint, app known to the registry", func(t *testing.T) {
		spec, a := setup(t, true)
		req := manifest.AppSpec{AppName: spec.AppName, AppStateFile: spec.AppStateFile}
		if rec := postJSON(t, handleDelete, req); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if process.IsAlive(spec, a) {
			t.Error("process still running")
		}
	})

	t.Run("delete with no entrypoint, app unknown to the registry", func(t *testing.T) {
		spec, a := setup(t, false)
		req := manifest.AppSpec{AppName: spec.AppName, AppStateFile: spec.AppStateFile}
		rec := postJSON(t, handleDelete, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if !process.IsAlive(spec, a) {
			t.Error("process was signaled although nothing identified it")
		}
		if _, err := os.Stat(spec.AppStateFile); !os.IsNotExist(err) {
			t.Errorf("state file still exists (err = %v)", err)
		}
		if msg := responseMessage(t, rec); !strings.Contains(msg, fmt.Sprintf("pid %d", a.PID)) {
			t.Errorf("message %q does not name pid %d", msg, a.PID)
		}
	})
}
