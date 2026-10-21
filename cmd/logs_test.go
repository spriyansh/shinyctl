/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer lets the test read the output while printLogs is still
// following and writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func writeLogsManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shinyctl.yaml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLogsPath_SkipsValidate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// With no Rscript on PATH and an entrypoint that doesn't exist,
	// Validate would reject this manifest on two counts.
	t.Setenv("PATH", t.TempDir())

	manifestFile := writeLogsManifest(t, `
app-group: g
app-name: n
entrypoint-file: ./gone.R
port: 8881
app-state-file: ./state.json
`)

	got, err := logsPath(manifestFile, false)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(home, ".shinyctl", "logs", "g-n.log")
	if got != want {
		t.Errorf("logsPath() = %q, want %q", got, want)
	}
}

func TestLogsPath_RequiresGroupAndName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for name, body := range map[string]string{
		"no group": "app-name: n\n",
		"no name":  "app-group: g\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := logsPath(writeLogsManifest(t, body), false)
			if err == nil || !strings.Contains(err.Error(), "app-group and app-name are required") {
				t.Errorf("logsPath() error = %v, want it to say app-group and app-name are required", err)
			}
		})
	}
}

func TestLogsPath_API(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := logsPath("", true)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(home, ".shinyctl", "api.log")
	if got != want {
		t.Errorf("logsPath(api) = %q, want %q", got, want)
	}
}

func TestPrintLogs_MissingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-ran.log")

	err := printLogs(t.Context(), &bytes.Buffer{}, path, 10, false, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no log yet at "+path) {
		t.Errorf("printLogs() error = %v, want %q", err, "no log yet at "+path)
	}
}

// Cancelling is how Ctrl-C ends --follow, and it has to come back as nil so
// the command exits 0.
func TestPrintLogs_FollowUntilCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(path, []byte("a\nb\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- printLogs(ctx, &out, path, 1, true, 5*time.Millisecond) }()

	waitForOutput := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for out.String() != want {
			if time.Now().After(deadline) {
				t.Fatalf("output = %q, want %q", out.String(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	waitForOutput("b\n")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("c\n"); err != nil {
		t.Fatal(err)
	}
	waitForOutput("b\nc\n")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("printLogs() = %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("printLogs() did not return after cancel")
	}
}
