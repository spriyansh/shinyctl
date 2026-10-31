package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

// TestExamples loads every example the way `shinyctl apply -f` does and
// validates it. CI has no R, so the fake Rscript stands in for the check that
// the interpreter exists. The examples are meant to run side by side, so
// they share a group and a state file name (.gitignore covers it), and
// their ports differ and stay out of the range shinyctl keeps for the apps'
// own processes.
func TestExamples(t *testing.T) {
	useFakeRscript(t)

	files, err := filepath.Glob("../examples/*/shinyctl.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 {
		t.Fatalf("found %d examples, want at least hello and memory", len(files))
	}

	ports := map[int]string{}
	for _, file := range files {
		spec, err := manifest.Load(file)
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if err := manifest.Validate(spec); err != nil {
			t.Errorf("%s: %v", file, err)
		}
		if spec.AppGroup != "examples" {
			t.Errorf("%s: app-group = %q, want examples", file, spec.AppGroup)
		}
		if base := filepath.Base(spec.AppStateFile); base != "state.json" {
			t.Errorf("%s: app-state-file is %q, want a state.json", file, base)
		}
		if spec.Port >= manifest.InternalPortMin && spec.Port <= manifest.InternalPortMax {
			t.Errorf("%s: port %d is in the range kept for the apps' own processes", file, spec.Port)
		}
		if other, taken := ports[spec.Port]; taken {
			t.Errorf("%s: port %d is also %s's", file, spec.Port, other)
		}
		ports[spec.Port] = file
	}
}

// TestExamplesInArchive: the release archive packs the files of each example
// by name and not the folder, so a state.json left in an example folder never
// ships; the price is that a new example must be listed.
func TestExamplesInArchive(t *testing.T) {
	conf, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dirs, err := os.ReadDir("../examples")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		for _, file := range []string{"app.R", "shinyctl.yaml"} {
			if want := "src: examples/" + d.Name() + "/" + file; !strings.Contains(string(conf), want) {
				t.Errorf(".goreleaser.yaml does not pack %q", want)
			}
		}
	}
}
