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
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/state"
)

// These tests are about moves of an app's address that the load balancer can
// or can't carry out. Widening a host (127.0.0.1 to 0.0.0.0) next to another
// program on a specific address is refused by Linux, where a wildcard socket
// can't sit beside a specific one, and allowed by macOS, so those tests have
// something to check on Linux only.

// lanIPv4 is a non-loopback IPv4 address of this machine.
func lanIPv4(t *testing.T) string {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			return ipn.IP.String()
		}
	}
	t.Skip("no non-loopback IPv4 address")
	return ""
}

// servedOnLoopback applies an app on 127.0.0.1 and returns it, running.
func servedOnLoopback(t *testing.T) (manifest.AppSpec, state.AppState) {
	t.Helper()
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	spec.Host = "127.0.0.1"
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
	})
	a = applyOK(t, spec)
	waitServed(t, spec, a.PID)
	return spec, a
}

// hold listens on addr:port, as another program would, until the test ends.
func hold(t *testing.T, addr string, port int) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", net.JoinHostPort(addr, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("holding %s:%d: %v", addr, port, err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func assertStillOnLoopback(t *testing.T, spec manifest.AppSpec, a state.AppState) {
	t.Helper()
	if applied, _ := appRegistry.Spec(spec.AppStateFile); applied.EffectiveHost() != "127.0.0.1" {
		t.Errorf("registry host = %q, want 127.0.0.1 kept", applied.EffectiveHost())
	}
	if got := loadApp(t, spec); got.PID != a.PID {
		t.Errorf("pid = %d, want the original %d", got.PID, a.PID)
	}
	waitServed(t, spec, a.PID)
	if !gateway.Serves(spec.AppStateFile, "127.0.0.1", spec.Port) {
		t.Error("the load balancer no longer serves the old address")
	}
}

func TestWidening_RefusedByTheProbe(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("macOS lets a wildcard socket sit beside a specific one: nothing to refuse")
	}
	spec, a := servedOnLoopback(t)
	hold(t, lanIPv4(t), spec.Port)

	wide := spec
	wide.Host = "0.0.0.0"
	rec := postJSON(t, handleApply, wide)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), fmt.Sprintf("%q is still on port %d", spec.AppName, spec.Port)) {
		t.Fatalf("status %d, body %q, want 409 naming the old port", rec.Code, rec.Body.String())
	}
	assertStillOnLoopback(t, spec, a)
}

func TestWidening_RolledBackWhenTheBindFails(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("macOS lets a wildcard socket sit beside a specific one: the bind doesn't fail")
	}
	spec, a := servedOnLoopback(t)
	hold(t, lanIPv4(t), spec.Port)
	useHooks(t, hooks{ProbeAlongside: func(string, int) error { return nil }})

	wide := spec
	wide.Host = "0.0.0.0"
	rec := postJSON(t, handleApply, wide)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "is still on port") {
		t.Fatalf("status %d, body %q, want 409 naming the old port", rec.Code, rec.Body.String())
	}
	assertStillOnLoopback(t, spec, a)
}

// TestWidening_RetryAfterRollbackGoesThrough: once the holder is gone, trying
// again works at once. The address that failed must not be held against the
// app, and must not be probed in a way its own old socket fails.
func TestWidening_RetryAfterRollbackGoesThrough(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("macOS lets a wildcard socket sit beside a specific one: there is no rollback to retry")
	}
	spec, a := servedOnLoopback(t)
	ln := hold(t, lanIPv4(t), spec.Port)
	wide := spec
	wide.Host = "0.0.0.0"

	useHooks(t, hooks{ProbeAlongside: func(string, int) error { return nil }})
	if rec := postJSON(t, handleApply, wide); rec.Code != http.StatusConflict {
		t.Fatalf("first try: status %d, body %q, want 409", rec.Code, rec.Body.String())
	}
	activeHooks.Store(nil)
	ln.Close()

	rec := postJSON(t, handleApply, wide)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry: status %d, body %q, want 200", rec.Code, rec.Body.String())
	}
	if !gateway.Serves(spec.AppStateFile, "0.0.0.0", spec.Port) {
		t.Error("the app is not served at 0.0.0.0 after the retry")
	}
	waitServed(t, wide, a.PID)
}

// TestNarrowing_IsNotRefused: narrowing next to the load balancer's own
// wildcard socket works on every system.
func TestNarrowing_IsNotRefused(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	spec.Host = "0.0.0.0"
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
	})
	a = applyOK(t, spec)
	waitServed(t, spec, a.PID)

	narrow := spec
	narrow.Host = "127.0.0.1"
	if rec := postJSON(t, handleApply, narrow); rec.Code != http.StatusOK {
		t.Fatalf("narrowing: status %d, body %q, want 200", rec.Code, rec.Body.String())
	}
	waitServed(t, narrow, a.PID)
}

// TestHandleRestart_MoveThatCannotBindChangesNothing is the restart's side of
// the same promise: the manifest it remembered is put back, so the app is
// still on the port it was on.
func TestHandleRestart_MoveThatCannotBindChangesNothing(t *testing.T) {
	useFakeRscript(t)
	ports := freePorts(t, 2)
	spec1 := newTestSpec(t, t.TempDir(), "app", "app.R", ports[0])
	var a, b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec1)
		killIfAlive(spec1, a)
		killIfAlive(spec1, b)
	})
	a = applyOK(t, spec1)
	waitServed(t, spec1, a.PID)

	hold(t, spec1.Host, ports[1])
	useHooks(t, hooks{Probe: func(string, int) error { return nil }})

	spec2 := spec1
	spec2.Port = ports[1]
	rec := postJSON(t, handleRestart, spec2)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), fmt.Sprintf("%q is still on port %d", spec1.AppName, spec1.Port)) {
		t.Fatalf("status = %d, body = %q, want 409 naming the old port", rec.Code, rec.Body.String())
	}
	if applied, _ := appRegistry.Spec(spec1.AppStateFile); applied.Port != spec1.Port {
		t.Errorf("registry port = %d, want the old %d put back", applied.Port, spec1.Port)
	}
	// The restart itself happened; the new process is served on the old port.
	b = loadApp(t, spec1)
	waitServed(t, spec1, b.PID)
}

// TestHandleApply_UnservedAppThatCannotBindIsAnError: an app that is not
// being served (stopped) has no listener to go back to, so a bind failure is
// the plain 500 and the manifest it asked for stays remembered.
func TestHandleApply_UnservedAppThatCannotBindIsAnError(t *testing.T) {
	useFakeRscript(t)
	ports := freePorts(t, 2)
	spec1 := newTestSpec(t, t.TempDir(), "app", "app.R", ports[0])
	var a, b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec1)
		killIfAlive(spec1, a)
		killIfAlive(spec1, b)
	})
	a = applyOK(t, spec1)
	if rec := postJSON(t, handleStop, spec1); rec.Code != http.StatusOK {
		t.Fatalf("stop: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	hold(t, spec1.Host, ports[1])
	useHooks(t, hooks{Probe: func(string, int) error { return nil }})
	spec2 := spec1
	spec2.Port = ports[1]
	rec := postJSON(t, handleApply, spec2)
	b = loadApp(t, spec2)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "is not available") {
		t.Errorf("status = %d, body = %q, want 500 with the bind error", rec.Code, rec.Body.String())
	}
	if applied, _ := appRegistry.Spec(spec1.AppStateFile); applied.Port != spec2.Port {
		t.Errorf("registry port = %d, want the requested %d kept", applied.Port, spec2.Port)
	}
}

// TestHandleApply_MoveOntoAnotherAppsPortIsRolledBack: the load balancer
// leaves the moving app out because another app has the address, not because
// of a bind error (here the check was made blind to it), and the move is
// undone all the same.
func TestHandleApply_MoveOntoAnotherAppsPortIsRolledBack(t *testing.T) {
	useFakeRscript(t)
	ports := freePorts(t, 2)
	specA := newTestSpec(t, t.TempDir(), "a", "app.R", ports[0])
	specB := newTestSpec(t, t.TempDir(), "b", "app.R", ports[1])
	var a, b state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, specA)
		postJSON(t, handleDelete, specB)
		killIfAlive(specA, a)
		killIfAlive(specB, b)
	})
	a = applyOK(t, specA)
	b = applyOK(t, specB)
	waitServed(t, specA, a.PID)
	waitServed(t, specB, b.PID)

	// The checks see the old address of the moving app and nothing of b.
	useHooks(t, hooks{
		Served: func() []proxy.App {
			var mine []proxy.App
			for _, s := range gateway.Served() {
				if s.Key == specA.AppStateFile {
					mine = append(mine, s)
				}
			}
			return mine
		},
		Probe: func(string, int) error { return nil },
	})

	moved := specA
	moved.Port = specB.Port
	rec := postJSON(t, handleApply, moved)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), fmt.Sprintf("%q is still on port %d", specA.AppName, specA.Port)) {
		t.Fatalf("status = %d, body = %q, want 409 naming the old port", rec.Code, rec.Body.String())
	}
	if applied, _ := appRegistry.Spec(specA.AppStateFile); applied.Port != specA.Port {
		t.Errorf("registry port = %d, want the old %d", applied.Port, specA.Port)
	}
	waitServed(t, specA, a.PID)
	waitServed(t, specB, b.PID)
}

// TestHandleStatus_ReportsWhyAPortIsNotServed: an app whose port the load
// balancer couldn't take says so in the status.
func TestHandleStatus_ReportsWhyAPortIsNotServed(t *testing.T) {
	useFakeRscript(t)
	port := freePorts(t, 1)[0]
	spec := newTestSpec(t, t.TempDir(), "blocked", "app.R", port)
	var a state.AppState
	t.Cleanup(func() {
		appRegistry.Forget(spec.AppStateFile)
		killIfAlive(spec, a)
	})

	// Registered and running with an internal port, but the public port was
	// taken by another program before the load balancer got to it.
	if err := appRegistry.Remember(spec); err != nil {
		t.Fatal(err)
	}
	a, _ = startRaw(t, spec)
	if err := state.Save(spec.AppStateFile, state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{a}}); err != nil {
		t.Fatal(err)
	}
	hold(t, spec.Host, port)
	if _, err := gateway.Sync(desiredApps()); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var resp api.StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, s := range resp.Apps {
		if s.Name == "blocked" {
			if !strings.Contains(s.NotServed, "is not available") {
				t.Errorf("NotServed = %q, want the bind error", s.NotServed)
			}
			return
		}
	}
	t.Error("the app is not in the status")
}
