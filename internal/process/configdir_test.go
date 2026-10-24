package process

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

func TestCopyLogPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	spec := manifest.AppSpec{AppGroup: "g", AppName: "app"}
	dir := filepath.Join(home, ".shinyctl", "logs")

	for n, want := range map[int]string{1: "g-app.log", 2: "g-app@2.log", 10: "g-app@10.log"} {
		got, err := CopyLogPath(spec, n)
		if err != nil || got != filepath.Join(dir, want) {
			t.Errorf("CopyLogPath(%d) = %q, %v; want %q", n, got, err, want)
		}
	}
	if first, _ := AppLogPath(spec); first != filepath.Join(dir, "g-app.log") {
		t.Errorf("AppLogPath = %q, want copy 1's", first)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("the logs directory was not created: %v", err)
	}
}

func TestAppLogPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".shinyctl", "logs")

	// Nothing yet, not even the directory.
	spec := manifest.AppSpec{AppGroup: "g", AppName: "app"}
	if got, err := AppLogPaths(spec); err != nil || len(got) != 0 {
		t.Errorf("no logs directory: %v, %v", got, err)
	}

	os.MkdirAll(dir, 0o700)
	for _, name := range []string{
		"g-app.log", "g-app@3.log", "g-app@2.log", "g-app@10.log",
		"g-app.log.1",      // rotated
		"g-app@2.log.1",    // rotated
		"g-app@1.log",      // copy 1 has the plain name
		"g-app@02.log",     // not what is written
		"g-app@x.log",      // not a number
		"g-app@.log",       // no number
		"g-apple.log",      // another app
		"g-app-more@2.log", // another app
		"h-app@2.log",      // another group
		"g-api.log",
	} {
		os.WriteFile(filepath.Join(dir, name), nil, 0o600)
	}

	got, err := AppLogPaths(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []CopyLog{
		{1, filepath.Join(dir, "g-app.log")},
		{2, filepath.Join(dir, "g-app@2.log")},
		{3, filepath.Join(dir, "g-app@3.log")},
		{10, filepath.Join(dir, "g-app@10.log")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AppLogPaths = %v, want %v", got, want)
	}
}

// Names are compared as text: an app whose name holds pattern characters
// finds its own logs, and not another app's.
func TestAppLogPaths_NameWithPatternCharacters(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".shinyctl", "logs")
	os.MkdirAll(dir, 0o700)

	spec := manifest.AppSpec{AppGroup: "g", AppName: "a[b]*"}
	for _, name := range []string{"g-a[b]*.log", "g-a[b]*@2.log", "g-ab.log", "g-abc@2.log", "g-a[b]x@2.log"} {
		os.WriteFile(filepath.Join(dir, name), nil, 0o600)
	}
	got, err := AppLogPaths(spec)
	if err != nil || len(got) != 2 || got[0].Copy != 1 || got[1].Copy != 2 {
		t.Errorf("AppLogPaths = %v, %v; want only the app's own two", got, err)
	}
}
