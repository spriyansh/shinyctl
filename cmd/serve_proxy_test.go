/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/state"
)

// useHooks substitutes the given hooks for the test.
func useHooks(t *testing.T, h hooks) {
	t.Helper()
	activeHooks.Store(&h)
	t.Cleanup(func() { activeHooks.Store(nil) })
}

// servedList is a Served hook for a fixed list.
func servedList(apps ...proxy.App) func() []proxy.App {
	return func() []proxy.App { return apps }
}

func TestClaimPort(t *testing.T) {
	probed := func() (func(string, int) error, *[]string) {
		var calls []string
		return func(host string, port int) error {
			calls = append(calls, fmt.Sprintf("%s:%d", host, port))
			return nil
		}, &calls
	}

	t.Run("the app's own address needs no check", func(t *testing.T) {
		probe, calls := probed()
		alongside, acalls := probed()
		spec := manifest.AppSpec{AppStateFile: "/x/a/state.json", Host: "127.0.0.1", Port: 8080}
		useHooks(t, hooks{
			Served:         servedList(proxy.App{Key: spec.AppStateFile, Host: "127.0.0.1", Port: 8080}),
			Probe:          probe,
			ProbeAlongside: alongside,
		})
		if err := claimPort(spec); err != nil {
			t.Fatal(err)
		}
		if len(*calls)+len(*acalls) != 0 {
			t.Errorf("probed %v %v, want no probe for an unchanged address", *calls, *acalls)
		}
	})

	t.Run("the same address written another way is unchanged", func(t *testing.T) {
		probe, calls := probed()
		spec := manifest.AppSpec{AppStateFile: "/x/a/state.json", Host: "0:0:0:0:0:0:0:1", Port: 8080}
		useHooks(t, hooks{
			Served: servedList(proxy.App{Key: spec.AppStateFile, Host: "::1", Port: 8080}),
			Probe:  probe,
		})
		if err := claimPort(spec); err != nil || len(*calls) != 0 {
			t.Errorf("claimPort = %v, probes %v, want nil and none", err, *calls)
		}
	})

	t.Run("a changed host on the same port is probed beside the app's own socket", func(t *testing.T) {
		probe, calls := probed()
		var alongside []string
		spec := manifest.AppSpec{AppStateFile: "/x/a/state.json", Host: "0.0.0.0", Port: 8080}
		useHooks(t, hooks{
			Served: servedList(proxy.App{Key: spec.AppStateFile, Host: "127.0.0.1", Port: 8080}),
			Probe:  probe,
			ProbeAlongside: func(host string, port int) error {
				alongside = append(alongside, fmt.Sprintf("%s:%d", host, port))
				return errors.New("0.0.0.0:8080 is not available (taken)")
			},
		})
		err := claimPort(spec)
		if err == nil || !strings.Contains(err.Error(), "is not available") {
			t.Fatalf("claimPort = %v, want the probe's error", err)
		}
		if len(alongside) != 1 || alongside[0] != "0.0.0.0:8080" || len(*calls) != 0 {
			t.Errorf("alongside probes %v, plain probes %v, want one alongside probe of 0.0.0.0:8080", alongside, *calls)
		}
	})

	t.Run("an address another app overlaps is named, whatever its spelling", func(t *testing.T) {
		other := manifest.AppSpec{AppGroup: "lab", AppName: "explorer", AppStateFile: "/x/other/state.json"}
		if err := appRegistry.Remember(other); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { appRegistry.Forget(other.AppStateFile) })

		spec := manifest.AppSpec{AppStateFile: "/x/a/state.json", Host: "0:0:0:0:0:0:0:1", Port: 8080}
		useHooks(t, hooks{Served: servedList(proxy.App{Key: other.AppStateFile, Host: "::1", Port: 8080})})
		want := `[::1]:8080 is already used by app "explorer" (app-group "lab") — pick a different port`
		if err := claimPort(spec); err == nil || err.Error() != want {
			t.Errorf("claimPort = %v, want %q", err, want)
		}
	})

	t.Run("an address nothing serves is probed, in canonical form", func(t *testing.T) {
		probe, calls := probed()
		spec := manifest.AppSpec{AppStateFile: "/x/a/state.json", Host: "::ffff:127.0.0.1", Port: 8080}
		useHooks(t, hooks{Served: servedList(), Probe: probe})
		if err := claimPort(spec); err != nil {
			t.Fatal(err)
		}
		if len(*calls) != 1 || (*calls)[0] != "127.0.0.1:8080" {
			t.Errorf("probes %v, want 127.0.0.1:8080", *calls)
		}
	})
}

// TestCheckMove_HostChange: a host change is a move like a port change, and
// is checked when it keeps the port (the check that compares only ports lets
// every host widening through).
func TestCheckMove_HostChange(t *testing.T) {
	spec := manifest.AppSpec{AppName: "app", AppStateFile: "/x/a/state.json", Host: "0.0.0.0", Port: 8080}
	useHooks(t, hooks{
		Served: servedList(proxy.App{Key: spec.AppStateFile, Host: "127.0.0.1", Port: 8080}),
		ProbeAlongside: func(host string, port int) error {
			return fmt.Errorf("%s:%d is not available (taken)", host, port)
		},
	})
	err := checkMove(spec)
	if err == nil || !strings.Contains(err.Error(), `is not available (taken); "app" is still on port 8080`) {
		t.Errorf("checkMove = %v, want a refusal naming the port the app is still on", err)
	}

	spec.Host = "127.0.0.1"
	if err := checkMove(spec); err != nil {
		t.Errorf("checkMove with the same host and port = %v, want nil", err)
	}
}

// freeInternalPort returns a port in the internal range nothing listens on.
func freeInternalPort(t *testing.T) int {
	t.Helper()
	for p := 28000; p < manifest.InternalPortMax; p++ {
		if process.PortAvailable(process.InternalHost, p) == nil {
			return p
		}
	}
	t.Fatal("no free internal port")
	return 0
}

// logBuffer captures the standard log for a test.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestCopyPort_WaitsForTheRecordedPort(t *testing.T) {
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	recorded := freeInternalPort(t)
	st := state.State{AppGroup: spec.AppGroup, Apps: []state.AppState{{Copy: 1, Port: recorded}}}
	useHooks(t, hooks{PortWait: 2 * time.Second, PortEvery: 20 * time.Millisecond})

	t.Run("a socket the OS releases a moment later is waited for", func(t *testing.T) {
		ln, err := net.Listen("tcp4", net.JoinHostPort(process.InternalHost, fmt.Sprint(recorded)))
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			time.Sleep(150 * time.Millisecond)
			ln.Close()
		}()
		got, err := copyPort(spec, &st, 1)
		if err != nil || got != recorded {
			t.Errorf("copyPort = %d, %v, want the recorded port %d kept", got, err, recorded)
		}
	})

	t.Run("a port that stays taken is replaced, and the log says why", func(t *testing.T) {
		useHooks(t, hooks{PortWait: 200 * time.Millisecond, PortEvery: 20 * time.Millisecond})
		ln, err := net.Listen("tcp4", net.JoinHostPort(process.InternalHost, fmt.Sprint(recorded)))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()

		var logs logBuffer
		log.SetOutput(&logs)
		defer log.SetOutput(os.Stderr)

		got, err := copyPort(spec, &st, 1)
		if err != nil || got == recorded || got < manifest.InternalPortMin || got > manifest.InternalPortMax {
			t.Fatalf("copyPort = %d, %v, want another internal port than %d", got, err, recorded)
		}
		want := fmt.Sprintf("port %d is taken (listen tcp4 127.0.0.1:%d: bind: address already in use); moving copy 1 to port %d", recorded, recorded, got)
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log = %q, want it to contain %q", logs.String(), want)
		}
		if saved := loadApp(t, spec); saved.Port != got || saved.Copy != 1 {
			t.Errorf("saved record = %+v, want the new port %d saved", saved, got)
		}
	})
}

// TestHandleApply_MoveThatCannotBindChangesNothing: the check passes (it
// can't see the holder), the load balancer can't bind the new port, and the
// app is back on its old one with the old manifest.
func TestHandleApply_MoveThatCannotBindChangesNothing(t *testing.T) {
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

	ln, err := net.Listen("tcp4", net.JoinHostPort(spec1.Host, fmt.Sprint(ports[1])))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	useHooks(t, hooks{Probe: func(string, int) error { return nil }})

	spec2 := spec1
	spec2.Port = ports[1]
	rec := postJSON(t, handleApply, spec2)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body = %s)", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"is not available", fmt.Sprintf("%q is still on port %d", spec1.AppName, spec1.Port)} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("body %q does not contain %q", rec.Body.String(), want)
		}
	}

	if applied, _ := appRegistry.Spec(spec1.AppStateFile); applied.Port != spec1.Port {
		t.Errorf("registry port = %d, want the old %d", applied.Port, spec1.Port)
	}
	if got := loadApp(t, spec1); got.PID != a.PID || !process.IsAlive(spec1, got) {
		t.Errorf("recorded app = %+v, want the original process %d", got, a.PID)
	}
	waitServed(t, spec1, a.PID)
}

// TestDesiredApps_LeavesOutOldStyleRecords: a process from before the load
// balancer holds the app's port itself, so the load balancer must not try to
// take it too; the upgrade moves it behind first.
func TestDesiredApps_LeavesOutOldStyleRecords(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "app", "app.R", freePorts(t, 1)[0])
	var a state.AppState
	t.Cleanup(func() {
		appRegistry.Forget(spec.AppStateFile)
		killIfAlive(spec, a)
	})
	a = startOldStyle(t, spec)

	for _, d := range desiredApps() {
		if d.Key == spec.AppStateFile {
			t.Errorf("desiredApps lists the old-style app: %+v", d)
		}
	}
}

// TestHandleStatus_OldStyleRecordReadyByItsOwnPort: until the upgrade moves
// it, a record from before the load balancer is ready when its own port
// answers, not when the load balancer serves it.
func TestHandleStatus_OldStyleRecordReadyByItsOwnPort(t *testing.T) {
	useFakeRscript(t)
	spec := newTestSpec(t, t.TempDir(), "old", "app.R", freePorts(t, 1)[0])
	var a state.AppState
	t.Cleanup(func() {
		appRegistry.Forget(spec.AppStateFile)
		killIfAlive(spec, a)
	})
	a = startOldStyle(t, spec)

	rec := httptest.NewRecorder()
	handleStatus(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var resp api.StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, s := range resp.Apps {
		if s.Name == "old" {
			if len(s.Copies) != 1 || !s.Copies[0].Alive || !s.Copies[0].Ready {
				t.Errorf("status = %+v, want one copy, alive and ready", s)
			}
			return
		}
	}
	t.Error("the old-style app is not in the status")
}

// TestAdminSocketPathAgrees: process.CheckSocketPaths checks the path the
// load balancer really binds.
func TestAdminSocketPathAgrees(t *testing.T) {
	config, err := process.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := process.ProxyDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(config, "proxy", "admin.sock"); proxy.SettingsFor(dir).AdminSocket != want {
		t.Errorf("admin socket = %s, want %s", proxy.SettingsFor(dir).AdminSocket, want)
	}
}
