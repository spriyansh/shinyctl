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
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/state"
)

// TestHandleApply_ReplacesPreviousInstance: a re-apply that changes the app
// name or the entrypoint starts a new copy next to the recorded one, which
// drains and stops. It stays in state.json until it does, so status sees it
// and delete stops it.
func TestHandleApply_ReplacesPreviousInstance(t *testing.T) {
	cases := []struct {
		name     string
		adopted  bool // first instance left behind by a previous daemon: no exit-watcher
		change   func(t *testing.T, old manifest.AppSpec, dir string, newPort int) manifest.AppSpec
		samePort bool
	}{
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
			name:     "app-name change on an app adopted after a shinyctl-api restart",
			adopted:  true,
			samePort: true,
			change: func(t *testing.T, old manifest.AppSpec, dir string, p int) manifest.AppSpec {
				old.AppName = "renamed"
				return old
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			useFakeRscript(t)
			settleNow(t)
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
				a = startCopy(t, spec1)
			} else {
				a = applyOK(t, spec1)
			}
			waitServed(t, spec1, a.PID)

			spec2 := c.change(t, spec1, dir, ports[1])

			rec := postJSON(t, handleApply, spec2)
			if rec.Code != http.StatusOK {
				t.Fatalf("apply: status = %d, body = %s", rec.Code, rec.Body.String())
			}

			// The old copy is not stopped by the apply: it drains once the
			// new one is ready and stops when it has no visitors.
			waitStopped(t, spec1, a)
			b := newestApp(t, spec2)
			if b.PID <= 0 || b.PID == a.PID || b.Copy == a.Copy {
				t.Fatalf("recorded app = %+v, want a new copy (old %d)", b, a.PID)
			}
			if b.Name != spec2.AppName {
				t.Errorf("recorded %q, want %q", b.Name, spec2.AppName)
			}
			if b.Port < manifest.InternalPortMin || b.Port > manifest.InternalPortMax {
				t.Errorf("recorded port = %d, want an internal one", b.Port)
			}
			if !process.IsAlive(spec2, b) {
				t.Errorf("new instance (pid %d) is not alive", b.PID)
			}
			waitServed(t, spec2, b.PID)
			if !c.samePort {
				waitClosed(t, spec1.Host, spec1.Port)
			}

			// The stopped process's exit must not be charged to its successor.
			assertNoExitRecorded(t, spec2, b.PID)

			if rec := postJSON(t, handleDelete, spec2); rec.Code != http.StatusOK {
				t.Fatalf("delete: status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if process.IsAlive(spec2, b) {
				t.Error("new instance survived delete")
			}
			waitClosed(t, spec2.Host, spec2.Port)
		})
	}
}

// TestHandleApply_PortChangeOntoTakenPort: a port change onto a port the
// load balancer can't take is refused before anything changes, so the app
// keeps serving where it was, with the old manifest.
func TestHandleApply_PortChangeOntoTakenPort(t *testing.T) {
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

	ln, err := net.Listen("tcp4", net.JoinHostPort(spec1.Host, strconv.Itoa(ports[1])))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	spec2 := spec1
	spec2.Port = ports[1]
	rec := postJSON(t, handleApply, spec2)
	if rec.Code != http.StatusConflict {
		t.Fatalf("apply onto a taken port: status = %d, want 409 (body = %s)", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"is not available", fmt.Sprintf("%q is still on port %d", spec1.AppName, spec1.Port)} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body %q does not contain %q", rec.Body.String(), want)
		}
	}

	if got := loadApp(t, spec1); got.PID != a.PID || got.CrashCount != 0 || !process.IsAlive(spec1, got) {
		t.Errorf("recorded app = %+v, want the original process %d still running, no crash", got, a.PID)
	}
	waitServed(t, spec1, a.PID)
	if applied, _ := appRegistry.Spec(spec1.AppStateFile); applied.Port != spec1.Port {
		t.Errorf("registry port = %d, want the old %d", applied.Port, spec1.Port)
	}
}

// TestHandleApply_StoppedAppOntoTakenPort: an app that isn't served has no
// listener to protect, so applying it onto a taken port fails. Nothing ran,
// so it is not a crash: none is recorded, and status says why the app isn't
// started.
func TestHandleApply_StoppedAppOntoTakenPort(t *testing.T) {
	useFakeRscript(t)
	dir := t.TempDir()
	ports := freePorts(t, 2)
	spec1 := newTestSpec(t, dir, "app", "app.R", ports[0])
	t.Cleanup(func() { postJSON(t, handleDelete, spec1) })

	applyOK(t, spec1)
	if rec := postJSON(t, handleStop, spec1); rec.Code != http.StatusOK {
		t.Fatalf("stop: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	ln, err := net.Listen("tcp4", net.JoinHostPort(spec1.Host, strconv.Itoa(ports[1])))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	spec2 := spec1
	spec2.Port = ports[1]
	rec := postJSON(t, handleApply, spec2)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "is not available") {
		t.Fatalf("apply onto a taken port: status = %d, body = %s, want 500 with the bind message", rec.Code, rec.Body.String())
	}

	time.Sleep(250 * time.Millisecond)
	app := loadApp(t, spec1)
	if app.PID != 0 || app.CrashCount != 0 || !app.LastExitAt.IsZero() || app.LastExitFailed {
		t.Errorf("recorded app = %+v, want pid 0 and no exit recorded: the port is not the app's fault", app)
	}
	if s := statusOf(t, "app"); !strings.Contains(s.NotStarted, "is not available") {
		t.Errorf("status = %+v, want NotStarted to say the port is taken", s)
	}
}

// TestHandleApply_RestartsOnManifestChange: a re-apply that changes a field
// shaping the process starts a new copy, even when the port and name stay the
// same; the running one drains.
func TestHandleApply_RestartsOnManifestChange(t *testing.T) {
	cases := []struct {
		field  string
		change func(t *testing.T, old manifest.AppSpec, dir string) manifest.AppSpec
	}{
		{"env", func(t *testing.T, old manifest.AppSpec, dir string) manifest.AppSpec {
			old.Env = map[string]string{"GREETING": "again"}
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
			settleNow(t)
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
			waitServed(t, spec1, a.PID)

			spec2 = c.change(t, spec1, dir)
			rec := postJSON(t, handleApply, spec2)
			if rec.Code != http.StatusOK {
				t.Fatalf("apply: status = %d, body = %s", rec.Code, rec.Body.String())
			}
			msg := responseMessage(t, rec)
			if want := "manifest changed: " + c.field + "):"; !strings.Contains(msg, want) {
				t.Errorf("message %q does not contain %q", msg, want)
			}
			if want := "starting copy 2;"; !strings.Contains(msg, want) {
				t.Errorf("message %q does not contain %q", msg, want)
			}
			if !strings.Contains(msg, "copy 1 keeps serving until the new one is ready") {
				t.Errorf("message %q does not say the old copy keeps serving", msg)
			}

			waitStopped(t, spec1, a)
			b = newestApp(t, spec2)
			if b.PID <= 0 || b.PID == a.PID {
				t.Fatalf("recorded pid = %d, want a new process (old %d)", b.PID, a.PID)
			}
			if !reflect.DeepEqual(b.StartedFrom, spec2.StartFields()) {
				t.Errorf("StartedFrom = %v, want %v", b.StartedFrom, spec2.StartFields())
			}
			waitServed(t, spec2, b.PID)
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
	waitServed(t, spec, a.PID)

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
	waitServed(t, spec, a.PID)

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

// TestHandleApply_OldStyleRecordMovesBehindLoadBalancer: a process from
// before the load balancer holds the app's own port. The first apply after
// the upgrade stops it and starts the app again behind the load balancer,
// which takes the port over.
func TestHandleApply_OldStyleRecordMovesBehindLoadBalancer(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var a, b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
		killIfAlive(spec, b)
	})
	a = startOldStyle(t, spec)

	rec := postJSON(t, handleApply, spec)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if process.IsAlive(spec, a) {
		t.Fatalf("the old-style process (pid %d) is still running", a.PID)
	}

	b = loadApp(t, spec)
	if b.PID <= 0 || b.PID == a.PID || b.Copy != 1 {
		t.Fatalf("recorded app = %+v, want a new process as copy 1 (old pid %d)", b, a.PID)
	}
	if b.Port < manifest.InternalPortMin || b.Port > manifest.InternalPortMax {
		t.Errorf("recorded port = %d, want an internal one", b.Port)
	}
	waitServed(t, spec, b.PID)

	want := fmt.Sprintf("moved behind the load balancer; stopped the previous instance: pid %d", a.PID)
	if msg := responseMessage(t, rec); !strings.Contains(msg, want) {
		t.Errorf("message %q does not contain %q", msg, want)
	}
	assertNoExitRecorded(t, spec, b.PID)
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
	waitServed(t, spec1, a.PID)

	if err := appRegistry.Remember(spec2); err != nil {
		t.Fatal(err)
	}
	settleNow(t)
	triggerReconcile(spec1.AppStateFile)

	waitStopped(t, spec1, a)
	b = newestApp(t, spec2)
	if b.PID <= 0 || b.PID == a.PID || !reflect.DeepEqual(b.StartedFrom, spec2.StartFields()) {
		t.Fatalf("recorded app = %+v, want a new process started from spec2", b)
	}
	waitServed(t, spec2, b.PID)
	assertNoExitRecorded(t, spec2, b.PID)
}

// TestTriggerReconcile_UpgradesOldStyleRecordOnce: after a shinyctl upgrade
// the first reconcile moves each running app behind the load balancer, and
// only that once.
func TestTriggerReconcile_UpgradesOldStyleRecordOnce(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var a, b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
		killIfAlive(spec, b)
	})
	a = startOldStyle(t, spec)

	triggerReconcile(spec.AppStateFile)

	if process.IsAlive(spec, a) {
		t.Fatalf("the old-style process (pid %d) is still running", a.PID)
	}
	b = loadApp(t, spec)
	if b.PID <= 0 || b.PID == a.PID || b.Copy != 1 {
		t.Fatalf("recorded app = %+v, want a new process as copy 1", b)
	}
	waitServed(t, spec, b.PID)

	triggerReconcile(spec.AppStateFile)
	if got := loadApp(t, spec); got.PID != b.PID || !process.IsAlive(spec, got) {
		t.Errorf("recorded app = %+v after a second reconcile, want pid %d unchanged", got, b.PID)
	}
}

// TestHandleApply_PortOrHostChangeKeepsProcess: the app's port and host say
// where the load balancer listens. Changing them moves the listener and
// leaves the process, and the sessions in it, alone.
func TestHandleApply_PortOrHostChangeKeepsProcess(t *testing.T) {
	cases := []struct {
		field    string
		change   func(s *manifest.AppSpec, newPort int)
		portMove bool
	}{
		{"port", func(s *manifest.AppSpec, p int) { s.Port = p }, true},
		{"host", func(s *manifest.AppSpec, p int) { s.Host = "0.0.0.0" }, false},
	}

	for _, c := range cases {
		t.Run(c.field, func(t *testing.T) {
			useFakeRscript(t)
			ports := freePorts(t, 2)
			spec1 := newTestSpec(t, t.TempDir(), "app", "app.R", ports[0])
			var a state.AppState
			t.Cleanup(func() {
				postJSON(t, handleDelete, spec1)
				killIfAlive(spec1, a)
			})
			a = applyOK(t, spec1)
			waitServed(t, spec1, a.PID)

			spec2 := spec1
			c.change(&spec2, ports[1])
			rec := postJSON(t, handleApply, spec2)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}

			want := fmt.Sprintf("Updated %q without a restart (manifest changed: %s)", "app", c.field)
			if msg := responseMessage(t, rec); msg != want {
				t.Errorf("message = %q, want %q", msg, want)
			}

			got := loadApp(t, spec2)
			if got.PID != a.PID || !process.IsAlive(spec2, got) {
				t.Errorf("recorded app = %+v, want the original process %d still running", got, a.PID)
			}
			if !reflect.DeepEqual(got.StartedFrom, a.StartedFrom) || got.Port != a.Port {
				t.Errorf("recorded start settings changed: %+v -> %+v", a, got)
			}
			waitServed(t, spec2, a.PID)
			if c.portMove {
				waitClosed(t, spec1.Host, spec1.Port)
			}
			assertNoExitRecorded(t, spec2, a.PID)
		})
	}
}

// TestHandleApply_StartsOnInternalPort: the process listens on 127.0.0.1 in
// the internal range, and the app's own port answers through the load
// balancer.
func TestHandleApply_StartsOnInternalPort(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	spec.Host = "" // the default: every interface, but only for the load balancer
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
	})
	a = applyOK(t, spec)

	if a.Copy != 1 || a.Port < manifest.InternalPortMin || a.Port > manifest.InternalPortMax {
		t.Fatalf("recorded app = %+v, want copy 1 on an internal port", a)
	}
	waitReady(t, process.InternalHost, a.Port)
	waitServed(t, spec, a.PID)
	if !process.IsReady("0.0.0.0", spec.Port) {
		t.Error("the app's own port does not answer")
	}
}

// TestHandleApply_PortUsedByAnotherApp: two apps on one port. The second
// fails with a message naming the first, no crash is recorded against it,
// and the first keeps serving.
func TestHandleApply_PortUsedByAnotherApp(t *testing.T) {
	useFakeRscript(t)
	port := freePorts(t, 1)[0]
	specA := newTestSpec(t, t.TempDir(), "explorer", "app.R", port)
	specA.AppGroup = "lab"
	specB := newTestSpec(t, t.TempDir(), "other", "app.R", port)
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, specA)
		postJSON(t, handleDelete, specB)
		killIfAlive(specA, a)
	})
	a = applyOK(t, specA)
	waitServed(t, specA, a.PID)

	rec := postJSON(t, handleApply, specB)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body = %s)", rec.Code, rec.Body.String())
	}
	want := fmt.Sprintf("127.0.0.1:%d is already used by app %q (app-group %q) — pick a different port", port, "explorer", "lab")
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	if b := loadApp(t, specB); b.PID != 0 || b.CrashCount != 0 || !b.LastExitAt.IsZero() {
		t.Errorf("recorded app B = %+v, want pid 0 and no crash", b)
	}
	if s := statusOf(t, "other"); s.NotStarted != want {
		t.Errorf("status of B: NotStarted = %q, want %q", s.NotStarted, want)
	}
	waitServed(t, specA, a.PID)
}

func TestHandleApply_ReservedPortRejected(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", 25000)
	if rec := postJSON(t, handleApply, spec); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body = %s)", rec.Code, rec.Body.String())
	}
}

// TestHandleApply_ConcurrentAppsGetDistinctPorts: apps starting at the same
// moment must not be handed the same internal port.
func TestHandleApply_ConcurrentAppsGetDistinctPorts(t *testing.T) {
	useFakeRscript(t)
	const n = 4
	ports := freePorts(t, n)
	specs := make([]manifest.AppSpec, n)
	for i := range specs {
		specs[i] = newTestSpec(t, t.TempDir(), fmt.Sprintf("app%d", i), "app.R", ports[i])
	}
	apps := make([]state.AppState, n)
	t.Cleanup(func() {
		for i, spec := range specs {
			postJSON(t, handleDelete, spec)
			killIfAlive(spec, apps[i])
		}
	})

	var wg sync.WaitGroup
	for i := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := postJSON(t, handleApply, specs[i]); rec.Code != http.StatusOK {
				t.Errorf("apply %d: status = %d, body = %s", i, rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()

	seen := map[int]int{}
	for i, spec := range specs {
		apps[i] = loadApp(t, spec)
		if j, dup := seen[apps[i].Port]; dup {
			t.Errorf("apps %d and %d both got internal port %d", j, i, apps[i].Port)
		}
		seen[apps[i].Port] = i
		waitServed(t, spec, apps[i].PID)
	}
}

// A control character in a path is refused by an apply, with its own message
// (not the missing-file one), but not by stop, which must still work for an
// app an earlier version accepted with such a path.
func TestHandleApply_ControlCharacterInAPath(t *testing.T) {
	useFakeRscript(t)
	dir := t.TempDir()
	spec := newTestSpec(t, dir, "app", "app.R", freePorts(t, 1)[0])
	odd := filepath.Join(dir, "odd\nname.R")
	if err := os.WriteFile(odd, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	spec.EntrypointFile = odd

	rec := postJSON(t, handleApply, spec)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "entrypoint-file must not contain control characters") {
		t.Errorf("apply: status %d, body %q; want 400 with the control-character message", rec.Code, rec.Body.String())
	}

	if rec := postJSON(t, handleStop, spec); rec.Code != http.StatusOK {
		t.Errorf("stop: status %d, body %q; want 200", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { postJSON(t, handleDelete, spec) })
}

// Restart remembers the spec and may start an app, so it refuses a control
// character in a path too: nothing is started or remembered.
func TestHandleRestart_ControlCharacterInAPath(t *testing.T) {
	useFakeRscript(t)
	dir := t.TempDir()
	spec := newTestSpec(t, dir, "app", "app.R", freePorts(t, 1)[0])
	odd := filepath.Join(dir, "odd\tname.R")
	if err := os.WriteFile(odd, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	spec.EntrypointFile = odd

	rec := postJSON(t, handleRestart, spec)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "entrypoint-file must not contain control characters") {
		t.Errorf("restart: status %d, body %q; want 400 with the control-character message", rec.Code, rec.Body.String())
	}
	if _, known := appRegistry.Spec(spec.AppStateFile); known {
		t.Error("the spec was remembered")
		appRegistry.Forget(spec.AppStateFile)
	}
	if recs := loadCopies(t, spec); len(recs) != 0 {
		t.Errorf("records = %+v, want nothing started", recs)
	}
}

// The commands refuse a control character before they ask for shinyctl-api,
// which they would otherwise start.
func TestCLI_ControlCharacterIsRefusedBeforeContactingTheDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	odd := filepath.Join(dir, "odd\nname.R")
	if err := os.WriteFile(odd, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	manifestFile := filepath.Join(dir, "shinyctl.yaml")
	body := "app-group: g\napp-name: n\nport: 18881\napp-state-file: ./state.json\nentrypoint-file: \"" + "odd\\nname.R" + "\"\n"
	if err := os.WriteFile(manifestFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	asked := false
	orig := ensureAPIRunning
	ensureAPIRunning = func() error { asked = true; return nil }
	defer func() { ensureAPIRunning = orig }()

	for _, command := range []string{"apply", "restart"} {
		asked = false
		rootCmd.SetArgs([]string{command, "-f", manifestFile})
		rootCmd.SetOut(io.Discard)
		rootCmd.SetErr(io.Discard)
		err := rootCmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "entrypoint-file must not contain control characters") {
			t.Errorf("%s: err = %v, want the control-character message", command, err)
		}
		if asked {
			t.Errorf("%s asked for shinyctl-api before checking the manifest", command)
		}
	}
}
