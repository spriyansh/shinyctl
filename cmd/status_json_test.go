/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/registry"
)

func jsonOf(t *testing.T, doc statusDoc) string {
	t.Helper()
	var out bytes.Buffer
	if err := writeStatusJSON(&out, doc); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestStatusJSON_Document(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)
	// The daemon's offset is kept in the table and dropped in the document.
	plus2 := time.FixedZone("CEST", 2*3600)
	two, one := 2, 1
	resp := api.StatusResponse{
		BalancerMemory: 41 << 20,
		MachineMemory:  &api.MachineMemory{TotalBytes: 16 << 30, AvailableBytes: 8 << 30, Pressure: "normal"},
		Apps: []api.AppStatus{
			{Name: "my-app", Group: "lab", Host: "127.0.0.1", Port: 8080, Replicas: 1, WarmReplicas: 1,
				RestartPolicy: "always", MaxMemoryBytes: 512 << 20, Copies: []api.CopyStatus{
					{Copy: 1, Role: "draining", PID: 61429, Alive: true, Ready: true, StartedAt: time.Date(2026, 11, 4, 11, 10, 11, 0, plus2),
						Sessions: &two, MemoryBytes: 142 << 20, Outdated: true, DrainEndsAt: now.Add(15*time.Minute - time.Second)},
					{Copy: 2, Role: "active", PID: 61502, Alive: true, Ready: true, StartedAt: time.Date(2026, 11, 4, 9, 58, 40, 0, time.UTC),
						Sessions: &one, MemoryBytes: 139 << 20},
				}},
			{Name: "sparse", Host: "127.0.0.1", Port: 8081, Copies: []api.CopyStatus{
				{Copy: 1, PID: 91},
			}},
		},
	}

	got := jsonOf(t, statusJSON(resp, now))
	want := `{
  "schema": 1,
  "time": "2026-11-04T10:00:00Z",
  "api": {
    "running": true
  },
  "apps": [
    {
      "name": "my-app",
      "group": "lab",
      "host": "127.0.0.1",
      "port": 8080,
      "stopped": false,
      "serving": true,
      "replicas": 1,
      "warm_replicas": 1,
      "restart_policy": "always",
      "max_memory_bytes": 536870912,
      "not_served": null,
      "not_started": null,
      "copies": [
        {
          "copy": 1,
          "role": "draining",
          "status": "alive",
          "readiness": "ready",
          "pid": 61429,
          "alive": true,
          "started_at": "2026-11-04T09:10:11Z",
          "crashes": 0,
          "sessions": 2,
          "memory_bytes": 148897792,
          "outdated": true,
          "skip_drain": false,
          "drain_ends_at": "2026-11-04T10:14:59Z",
          "last_exit_reason": null,
          "page_problem": null,
          "page_waiting_since": null
        },
        {
          "copy": 2,
          "role": "active",
          "status": "alive",
          "readiness": "ready",
          "pid": 61502,
          "alive": true,
          "started_at": "2026-11-04T09:58:40Z",
          "crashes": 0,
          "sessions": 1,
          "memory_bytes": 145752064,
          "outdated": false,
          "skip_drain": false,
          "drain_ends_at": null,
          "last_exit_reason": null,
          "page_problem": null,
          "page_waiting_since": null
        }
      ],
      "notes": [
        "copy 1 is draining with 2 sessions; it stops when they end, or in 15m at the latest."
      ]
    },
    {
      "name": "sparse",
      "group": null,
      "host": "127.0.0.1",
      "port": 8081,
      "stopped": false,
      "serving": false,
      "replicas": 0,
      "warm_replicas": 0,
      "restart_policy": null,
      "max_memory_bytes": null,
      "not_served": null,
      "not_started": null,
      "copies": [
        {
          "copy": 1,
          "role": null,
          "status": "dead",
          "readiness": null,
          "pid": null,
          "alive": false,
          "started_at": null,
          "crashes": 0,
          "sessions": null,
          "memory_bytes": null,
          "outdated": false,
          "skip_drain": false,
          "drain_ends_at": null,
          "last_exit_reason": null,
          "page_problem": null,
          "page_waiting_since": null
        }
      ],
      "notes": []
    }
  ],
  "load_balancer": {
    "starting": false,
    "memory_bytes": 42991616
  },
  "machine": {
    "low": false,
    "total_bytes": 17179869184,
    "available_bytes": 8589934592,
    "swap_total_bytes": 0,
    "swap_used_bytes": 0,
    "stall_percent": null,
    "pressure": "normal"
  },
  "notes": [
    "Memory: 281M in 2 R processes (1 draining), plus 41M for the load balancer."
  ]
}
`
	if got != want {
		t.Errorf("document =\n%s\nwant\n%s", got, want)
	}
}

func TestStatusJSON_Nulls(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

	// No reading and no balancer memory: null, not an object of zeros.
	doc := statusJSON(api.StatusResponse{Apps: []api.AppStatus{}}, now)
	if doc.Machine != nil || doc.LoadBalancer == nil || doc.LoadBalancer.MemoryBytes != nil {
		t.Errorf("machine = %+v, load balancer = %+v", doc.Machine, doc.LoadBalancer)
	}
	out := jsonOf(t, doc)
	for _, want := range []string{`"apps": []`, `"notes": []`, `"machine": null`, `"memory_bytes": null`} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("document lacks %s:\n%s", want, out)
		}
	}

	// A Linux reading has a stall and no pressure.
	doc = statusJSON(api.StatusResponse{MachineMemory: &api.MachineMemory{Low: true, TotalBytes: 1 << 30, StallPercent: 12}}, now)
	if m := doc.Machine; m == nil || m.StallPercent == nil || *m.StallPercent != 12 || m.Pressure != nil || !m.Low {
		t.Errorf("linux machine = %+v", m)
	}
	// A macOS reading has a pressure and no stall.
	doc = statusJSON(api.StatusResponse{MachineMemory: &api.MachineMemory{Pressure: "warning"}}, now)
	if m := doc.Machine; m == nil || m.StallPercent != nil || m.Pressure == nil || *m.Pressure != "warning" {
		t.Errorf("mac machine = %+v", m)
	}
	// A nil Apps and no notes are still lists.
	doc = statusJSON(api.StatusResponse{}, now)
	if doc.Apps == nil || doc.Notes == nil {
		t.Errorf("apps = %v, notes = %v, want empty lists", doc.Apps, doc.Notes)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(jsonOf(t, doc)), &back); err != nil {
		t.Fatal(err)
	}
	if a, ok := back["apps"].([]any); !ok || len(a) != 0 {
		t.Errorf("apps = %v", back["apps"])
	}
}

func TestServing(t *testing.T) {
	ready := func(copy int, role string) api.CopyStatus {
		return api.CopyStatus{Copy: copy, Role: role, Alive: true, Ready: true}
	}
	cases := []struct {
		name string
		app  api.AppStatus
		want bool
	}{
		{"active and ready", api.AppStatus{Copies: []api.CopyStatus{ready(1, "active")}}, true},
		{"active among others", api.AppStatus{Copies: []api.CopyStatus{ready(1, "draining"), ready(2, "active")}}, true},
		{"only warm", api.AppStatus{Copies: []api.CopyStatus{ready(1, "warm")}}, false},
		{"only draining", api.AppStatus{Copies: []api.CopyStatus{ready(1, "draining")}}, false},
		{"copy 0 from before the load balancer", api.AppStatus{Copies: []api.CopyStatus{ready(0, "")}}, true},
		{"starting", api.AppStatus{Copies: []api.CopyStatus{{Copy: 1, Role: "active", Alive: true}}}, false},
		{"page failing", api.AppStatus{Copies: []api.CopyStatus{{Copy: 1, Role: "active", Alive: true, PageProblem: "answers HTTP 500"}}}, false},
		{"stopped", api.AppStatus{Stopped: true, Copies: []api.CopyStatus{ready(1, "active")}}, false},
		{"port in use", api.AppStatus{NotStarted: "taken", Copies: []api.CopyStatus{{Copy: 1}}}, false},
		{"no copies", api.AppStatus{}, false},
	}
	for _, tc := range cases {
		if got := serving(tc.app); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStatusJSON_NotRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	now := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

	doc, err := notRunningJSON(path, now)
	if err != nil {
		t.Fatalf("missing registry: %v", err)
	}
	if doc.Apps == nil || len(doc.Apps) != 0 || doc.API.Running || doc.Notes[0] != notRunningLine(path) {
		t.Errorf("missing registry: %+v", doc)
	}

	reg, err := registry.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old", "memory", "hello"} {
		spec := manifest.AppSpec{AppGroup: "g", AppName: name, Port: 8080, AppStateFile: filepath.Join(dir, name+".json")}
		if err := reg.Remember(spec); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.Stop(filepath.Join(dir, "old.json")); err != nil {
		t.Fatal(err)
	}

	doc, err = notRunningJSON(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if doc.API.Running || doc.LoadBalancer != nil || doc.Machine != nil || doc.Time != "2026-11-04T10:00:00Z" {
		t.Errorf("document = %+v", doc)
	}
	if len(doc.Notes) != 1 || doc.Notes[0] != notRunningLine(path) {
		t.Errorf("notes = %q, want the not-running line", doc.Notes)
	}
	wantNames, wantStopped := []string{"hello", "memory", "old"}, []bool{false, false, true}
	if len(doc.Apps) != 3 {
		t.Fatalf("apps = %+v", doc.Apps)
	}
	for i, a := range doc.Apps {
		if a.Name != wantNames[i] || a.Stopped != wantStopped[i] {
			t.Errorf("app %d = %s stopped %v, want %s stopped %v", i, a.Name, a.Stopped, wantNames[i], wantStopped[i])
		}
		if a.Serving != nil || a.NotServed != nil || a.NotStarted != nil || a.Copies == nil || len(a.Copies) != 0 || a.Notes == nil {
			t.Errorf("app %s: not inspected fields = %+v", a.Name, a)
		}
		if a.Group == nil || *a.Group != "g" || a.Host != "0.0.0.0" || a.Replicas != 1 || a.RestartPolicy == nil || *a.RestartPolicy != "always" {
			t.Errorf("app %s: defaults = %+v", a.Name, a)
		}
	}

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := notRunningJSON(path, now); err == nil {
		t.Error("a corrupt registry gave no error")
	}
}

// statusCommandHome points HOME at a directory short enough for a socket path
// on macOS, and returns the directory shinyctl keeps its files in.
func statusCommandHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "sj")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".shinyctl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// serveStatus answers on the api socket in dir with handler until the test ends.
func serveStatus(t *testing.T, dir string, handler http.HandlerFunc) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(dir, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// runStatusJSON runs shinyctl status --json with buffers of its own.
func runStatusJSON(t *testing.T) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	statusAsJSON = false
	rootCmd.SetArgs([]string{"status", "--json"})
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	err = rootCmd.Execute()
	return out.String(), errOut.String(), err
}

func TestStatusCommand_JSON(t *testing.T) {
	t.Cleanup(func() {
		statusAsJSON = false
		statusCmd.Flags().Lookup("json").Changed = false
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	prev := ensureAPIRunning
	t.Cleanup(func() { ensureAPIRunning = prev })
	ensureAPIRunning = func() error {
		t.Error("status started shinyctl-api")
		return nil
	}

	topKeys := []string{"schema", "time", "api", "apps", "load_balancer", "machine", "notes"}
	decode := func(t *testing.T, out string) map[string]any {
		t.Helper()
		if !strings.HasSuffix(out, "\n") {
			t.Errorf("output does not end in a newline: %q", out)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("output is not one JSON document: %v\n%s", err, out)
		}
		for _, k := range topKeys {
			if _, ok := doc[k]; !ok {
				t.Errorf("document lacks %q", k)
			}
		}
		if len(doc) != len(topKeys) {
			t.Errorf("document has %d keys, want %d", len(doc), len(topKeys))
		}
		return doc
	}

	t.Run("running", func(t *testing.T) {
		dir := statusCommandHome(t)
		serveStatus(t, dir, func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(api.StatusResponse{Apps: []api.AppStatus{
				{Name: "my-app", Group: "lab", Port: 8080, Copies: []api.CopyStatus{{Copy: 1, Role: "active", PID: 7, Alive: true, Ready: true}}},
			}})
		})
		stdout, stderr, err := runStatusJSON(t)
		if err != nil || stderr != "" {
			t.Fatalf("err = %v, stderr = %q", err, stderr)
		}
		doc := decode(t, stdout)
		if running := doc["api"].(map[string]any)["running"]; running != true {
			t.Errorf("api.running = %v", running)
		}
		apps := doc["apps"].([]any)
		if len(apps) != 1 || apps[0].(map[string]any)["serving"] != true {
			t.Errorf("apps = %v", apps)
		}
	})

	t.Run("running with no apps is still a document", func(t *testing.T) {
		dir := statusCommandHome(t)
		serveStatus(t, dir, func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(api.StatusResponse{Apps: []api.AppStatus{}})
		})
		stdout, _, err := runStatusJSON(t)
		if err != nil {
			t.Fatal(err)
		}
		if apps := decode(t, stdout)["apps"].([]any); len(apps) != 0 {
			t.Errorf("apps = %v", apps)
		}
	})

	t.Run("not running starts nothing", func(t *testing.T) {
		dir := statusCommandHome(t)
		stdout, stderr, err := runStatusJSON(t)
		if err != nil || stderr != "" {
			t.Fatalf("err = %v, stderr = %q", err, stderr)
		}
		doc := decode(t, stdout)
		if running := doc["api"].(map[string]any)["running"]; running != false {
			t.Errorf("api.running = %v", running)
		}
		if _, err := os.Stat(filepath.Join(dir, "api.sock")); !os.IsNotExist(err) {
			t.Errorf("api.sock after status: %v", err)
		}
	})

	t.Run("a 500 is an error and no document", func(t *testing.T) {
		dir := statusCommandHome(t)
		serveStatus(t, dir, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		stdout, stderr, err := runStatusJSON(t)
		if err == nil || stdout != "" || !strings.Contains(stderr, "shinyctl-api:") {
			t.Errorf("err = %v, stdout = %q, stderr = %q", err, stdout, stderr)
		}
	})

	t.Run("an unreadable registry is an error and no document", func(t *testing.T) {
		dir := statusCommandHome(t)
		if err := os.WriteFile(filepath.Join(dir, "registry.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, err := runStatusJSON(t)
		if err == nil || stdout != "" || !strings.Contains(stderr, "Error:") {
			t.Errorf("err = %v, stdout = %q, stderr = %q", err, stdout, stderr)
		}
	})
}
