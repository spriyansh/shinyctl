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
	app := got.Copies()[0]
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
	if app, ok := got.Copy(1); !ok || app.Port != 20001 {
		t.Errorf("Copy(1) = %+v, %v, want copy 1 on port 20001", app, ok)
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
	if app, ok := got.Copy(0); !ok || app.Port != 8881 {
		t.Errorf("Copy(0) = %+v, %v, want copy 0 on port 8881", app, ok)
	}

	if err := Save(path, got); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), `"copy"`) {
		t.Errorf("a record without a copy was written with one:\n%s", data)
	}
}

func TestCopies_MultiCopyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	want := State{AppGroup: "g", Revision: 2, Apps: []AppState{
		{Name: "a", PID: 1, Port: 20000, Copy: 1, Revision: 2},
		{Name: "a", PID: 2, Port: 20001, Copy: 2, Revision: 1},
	}}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}

func TestState_PutKeepsOrderAndReplaces(t *testing.T) {
	var s State
	s.Put(AppState{Copy: 3, PID: 3})
	s.Put(AppState{Copy: 1, PID: 1})
	s.Put(AppState{Copy: 2, PID: 2})
	s.Put(AppState{Copy: 2, PID: 22})

	var order []int
	for _, a := range s.Apps {
		order = append(order, a.Copy)
	}
	if !reflect.DeepEqual(order, []int{1, 2, 3}) {
		t.Errorf("Apps are in order %v, want 1 2 3", order)
	}
	if a, ok := s.Copy(2); !ok || a.PID != 22 {
		t.Errorf("Copy(2) = %+v, %v; want the replaced record", a, ok)
	}
	if _, ok := s.Copy(4); ok {
		t.Error("Copy(4) found a record")
	}
}

func TestState_Remove(t *testing.T) {
	s := State{Apps: []AppState{{Copy: 1}, {Copy: 2}, {Copy: 3}}}
	s.Remove(2)
	s.Remove(9)
	if _, ok := s.Copy(2); ok || len(s.Apps) != 2 {
		t.Errorf("Apps = %+v, want copies 1 and 3", s.Apps)
	}
}

func TestState_CopiesIsASortedCopy(t *testing.T) {
	s := State{Apps: []AppState{{Copy: 2}, {Copy: 1}}}
	got := s.Copies()
	if got[0].Copy != 1 || got[1].Copy != 2 {
		t.Errorf("Copies() = %+v", got)
	}
	if s.Apps[0].Copy != 2 {
		t.Error("Copies() sorted the state's own slice")
	}
}

// A file written before there were several copies is copy 1 at revision 0.
func TestLoad_FileBeforeSeveralCopiesIsOneCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"app-group":"g","apps":[{"name":"a","pid":5,"port":20000,"copy":1,"entrypoint-file":"/srv/app.R","started-at":"2026-10-01T00:00:00Z","crash-count":0,"last-exit-at":"0001-01-01T00:00:00Z","last-exit-failed":false}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	copies := got.Copies()
	if len(copies) != 1 || copies[0].Copy != 1 || copies[0].Revision != 0 || got.Revision != 0 {
		t.Errorf("Copies() = %+v, revision %d", copies, got.Revision)
	}
}

func TestSave_OmitsRevisionWhenZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, State{AppGroup: "g", Apps: []AppState{{Name: "a", Copy: 1}}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "revision") {
		t.Errorf("a state at revision 0 wrote a revision: %s", data)
	}
	if err := Save(path, State{AppGroup: "g", Revision: 1, Apps: []AppState{{Name: "a", Copy: 1, Revision: 1}}}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Count(string(data), `"revision"`) != 2 {
		t.Errorf("revision 1 should be written twice: %s", data)
	}
}

// A record without draining-since and skip-drain is a copy that is not
// draining; a copy that is not draining writes neither key; both round trip.
func TestDrainFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"app-group":"g","apps":[{"name":"a","pid":1,"copy":1,"port":20001}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	st, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if app, _ := st.Copy(1); !app.DrainingSince.IsZero() || app.SkipDrain {
		t.Errorf("a record without the keys = %+v, want not draining and unmarked", app)
	}
	if err := Save(path, st); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "draining-since") || strings.Contains(string(data), "skip-drain") {
		t.Errorf("a copy that is not draining wrote its keys: %s", data)
	}

	since := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	st = State{AppGroup: "g", Apps: []AppState{{Name: "a", PID: 1, Copy: 1, Port: 20001, DrainingSince: since, SkipDrain: true}}}
	if err := Save(path, st); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if app, _ := got.Copy(1); !app.DrainingSince.Equal(since) || !app.SkipDrain {
		t.Errorf("round trip = %+v, want draining since %v and marked", app, since)
	}
}
