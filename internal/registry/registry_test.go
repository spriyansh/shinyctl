package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

func TestRegistry(t *testing.T) {
	r := New()

	r.Remember(manifest.AppSpec{AppName: "a", AppStateFile: "a.json", Port: 8881})
	r.Remember(manifest.AppSpec{AppName: "b", AppStateFile: "b.json", Port: 8882})

	if got := len(r.All()); got != 2 {
		t.Fatalf("All() returned %d specs, want 2", got)
	}

	// Re-applying the same app (same state file) overwrites, not duplicates.
	r.Remember(manifest.AppSpec{AppName: "a", AppStateFile: "a.json", Port: 9999})

	all := r.All()
	if len(all) != 2 {
		t.Fatalf("All() returned %d specs after overwrite, want 2", len(all))
	}

	for _, spec := range all {
		if spec.AppStateFile == "a.json" && spec.Port != 9999 {
			t.Errorf("app %q: port = %d, want 9999 (overwritten)", spec.AppName, spec.Port)
		}
	}
}

func TestRegistry_StopAndResume(t *testing.T) {
	r := New()
	r.Remember(manifest.AppSpec{AppName: "a", AppStateFile: "a.json", Port: 8881})

	if r.IsStopped("a.json") {
		t.Fatal("IsStopped = true before Stop was ever called")
	}

	r.Stop("a.json")
	if !r.IsStopped("a.json") {
		t.Error("IsStopped = false after Stop")
	}

	// Re-applying (Remember) is what un-stops it — that's the whole point:
	// apply declares "this should be running," overriding a prior stop.
	r.Remember(manifest.AppSpec{AppName: "a", AppStateFile: "a.json", Port: 8881})
	if r.IsStopped("a.json") {
		t.Error("IsStopped = true after re-applying, want false (apply should un-stop)")
	}
}

// TestRegistry_SurvivesReopen is the restart scenario: everything one
// shinyctl-api remembered — specs and stopped flags both — must come back
// in the next one, or a restart silently turns the restart-on-crash
// promise off for every app until someone notices and re-applies.
func TestRegistry_SurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")

	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(r.All()); got != 0 {
		t.Fatalf("fresh Open: All() returned %d specs, want 0", got)
	}

	a := manifest.AppSpec{AppName: "a", AppStateFile: "/x/a.json", Port: 8881, Env: map[string]string{"K": "v"}}
	b := manifest.AppSpec{AppName: "b", AppStateFile: "/x/b.json", Port: 8882}
	if err := r.Remember(a); err != nil {
		t.Fatal(err)
	}
	if err := r.Remember(b); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(b.AppStateFile); err != nil {
		t.Fatal(err)
	}

	// A brand-new Registry from the same file, as a restarted process
	// would build.
	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	all := r2.All()
	if len(all) != 2 {
		t.Fatalf("after reopen: All() returned %d specs, want 2", len(all))
	}
	for _, spec := range all {
		switch spec.AppStateFile {
		case a.AppStateFile:
			if spec.AppName != "a" || spec.Port != 8881 || spec.Env["K"] != "v" {
				t.Errorf("spec a came back as %+v", spec)
			}
		case b.AppStateFile:
			if spec.AppName != "b" || spec.Port != 8882 {
				t.Errorf("spec b came back as %+v", spec)
			}
		default:
			t.Errorf("unexpected spec %+v", spec)
		}
	}

	if r2.IsStopped(a.AppStateFile) {
		t.Error("a came back stopped, was never stopped")
	}
	if !r2.IsStopped(b.AppStateFile) {
		t.Error("b came back running, was stopped before the reopen")
	}

	// And un-stopping persists too.
	if err := r2.Remember(b); err != nil {
		t.Fatal(err)
	}
	r3, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if r3.IsStopped(b.AppStateFile) {
		t.Error("b still stopped after re-apply + reopen")
	}
}

func TestRegistry_ManagedAndForget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	r, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	if r.Managed("a.json") {
		t.Error("Managed = true for an app never applied")
	}

	if err := r.Remember(manifest.AppSpec{AppName: "a", AppStateFile: "a.json"}); err != nil {
		t.Fatal(err)
	}
	if !r.Managed("a.json") {
		t.Error("Managed = false right after Remember")
	}

	if err := r.Stop("a.json"); err != nil {
		t.Fatal(err)
	}
	if r.Managed("a.json") {
		t.Error("Managed = true after Stop")
	}

	if err := r.Forget("a.json"); err != nil {
		t.Fatal(err)
	}
	if r.Managed("a.json") || r.IsStopped("a.json") || len(r.All()) != 0 {
		t.Errorf("after Forget: Managed=%v IsStopped=%v All=%v, want all empty/false",
			r.Managed("a.json"), r.IsStopped("a.json"), r.All())
	}

	// Forget persists: a reopen must not bring the app (or its stopped
	// flag) back.
	r2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.All()) != 0 || r2.IsStopped("a.json") {
		t.Errorf("after reopen: All=%v IsStopped=%v, want forgotten", r2.All(), r2.IsStopped("a.json"))
	}

	// Forgetting something unknown is fine.
	if err := r2.Forget("never-seen.json"); err != nil {
		t.Errorf("Forget(unknown) = %v, want nil", err)
	}

	// And a later Remember starts clean.
	if err := r2.Remember(manifest.AppSpec{AppName: "a", AppStateFile: "a.json"}); err != nil {
		t.Fatal(err)
	}
	if !r2.Managed("a.json") {
		t.Error("Managed = false after re-Remember following a Forget")
	}
}

func TestRegistry_OpenRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); err == nil {
		t.Error("Open(corrupt file) = nil error, want an error — never silently start empty")
	}
}

func TestRegistry_InMemoryWritesNothing(t *testing.T) {
	dir := t.TempDir()
	r := New()
	if err := r.Remember(manifest.AppSpec{AppName: "a", AppStateFile: filepath.Join(dir, "a.json")}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("New() registry wrote files: %v", entries)
	}
}
