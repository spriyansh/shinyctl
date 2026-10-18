/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"net"
	"net/http"
	"strconv"
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
