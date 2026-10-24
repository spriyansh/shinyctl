package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := State{AppGroup: "g", Apps: []AppState{{Name: "a", PID: 42, Port: 8881, EntrypointFile: "/srv/app.R", CrashCount: 2, StartedFrom: map[string]string{"host": "127.0.0.1", "env": "sha256:ab"}}}}

	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AppGroup != want.AppGroup || len(got.Apps) != 1 || !reflect.DeepEqual(got.Apps[0], want.Apps[0]) {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("mode = %o, want 0600", mode)
	}
}

func TestSaveLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	for i := 0; i < 3; i++ {
		if err := Save(path, State{AppGroup: "g"}); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir contains %v, want only state.json", names)
	}
}

// TestSaveReplacesExistingFileWholesale guards the atomic-rename property
// from the reader's side: a Save of a shorter document over a longer one
// must never leave a tail of the old bytes behind, which a plain
// truncate-and-write could if interrupted and which Load would then
// reject forever.
func TestSaveReplacesExistingFileWholesale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	long := State{AppGroup: "g"}
	for i := 0; i < 10; i++ {
		long.Apps = append(long.Apps, AppState{Name: "app-with-a-long-name", PID: i})
	}
	if err := Save(path, long); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, State{AppGroup: "g"}); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after shrinking Save: %v", err)
	}
	if len(got.Apps) != 0 {
		t.Errorf("Apps = %v, want none", got.Apps)
	}
}

func TestLoadMissingFileIsEmptyState(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.AppGroup != "" || len(got.Apps) != 0 {
		t.Errorf("Load() = %+v, want zero State", got)
	}
}

// TestLockSerializesReadModifyWrite runs the exact pattern every
// shinyctl-api goroutine uses — Load, bump a counter, Save — from many
// goroutines at once. Under Lock, no update is ever lost; without it,
// this test loses updates almost every run.
func TestLockSerializesReadModifyWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{Apps: []AppState{{}}}); err != nil {
		t.Fatal(err)
	}

	const workers, perWorker = 8, 25

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				func() {
					unlock := Lock(path)
					defer unlock()

					s, err := Load(path)
					if err != nil {
						t.Error(err)
						return
					}
					s.Apps[0].CrashCount++
					if err := Save(path, s); err != nil {
						t.Error(err)
					}
				}()
			}
		}()
	}
	wg.Wait()

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Apps[0].CrashCount != workers*perWorker {
		t.Errorf("CrashCount = %d, want %d (updates were lost)", got.Apps[0].CrashCount, workers*perWorker)
	}
}

// TestLockIsPerPath: holding one app's lock must not stall another app's
// reconcile — the ticker walks every app in sequence.
func TestLockIsPerPath(t *testing.T) {
	unlockA := Lock("/a/state.json")
	defer unlockA()

	acquired := make(chan struct{})
	go func() {
		unlockB := Lock("/b/state.json")
		unlockB()
		close(acquired)
	}()

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("Lock on a different path blocked behind an unrelated held lock")
	}
}

// TestLoadStateWithoutEntrypointFile: state.json files written before
// entrypoint-file was recorded must keep loading. The on-disk shape is a
// compatibility surface.
func TestLoadStateWithoutEntrypointFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{
  "app-group": "g",
  "apps": [
    {
      "name": "a",
      "pid": 42,
      "port": 8881,
      "started-at": "2026-01-02T03:04:05Z",
      "crash-count": 2,
      "last-exit-at": "2026-01-02T04:00:00Z",
      "last-exit-failed": true
    }
  ]
}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	app := got.App()
	if app.EntrypointFile != "" {
		t.Errorf("EntrypointFile = %q, want empty", app.EntrypointFile)
	}
	if app.StartedFrom != nil {
		t.Errorf("StartedFrom = %v, want nil", app.StartedFrom)
	}
	if app.Name != "a" || app.PID != 42 || app.Port != 8881 || app.CrashCount != 2 || !app.LastExitFailed {
		t.Errorf("other fields lost: %+v", app)
	}
}

func TestCopyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := State{AppGroup: "g", Apps: []AppState{{Name: "a", PID: 42, Copy: 1, Port: 20001}}}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.App().Copy != 1 || got.App().Port != 20001 {
		t.Errorf("App() = %+v, want copy 1 on port 20001", got.App())
	}
}

// TestCopyIsOptional: a state file from before the load balancer has no
// copy, and reads as copy 0; a record with copy 0 writes no key, so nothing
// changes for a file that was never upgraded.
func TestCopyIsOptional(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"app-group":"g","apps":[{"name":"a","pid":42,"port":8881,"entrypoint-file":"/srv/app.R"}]}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.App().Copy != 0 || got.App().Port != 8881 {
		t.Errorf("App() = %+v, want copy 0 on port 8881", got.App())
	}

	if err := Save(path, got); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), `"copy"`) {
		t.Errorf("a record without a copy was written with one:\n%s", data)
	}
}
