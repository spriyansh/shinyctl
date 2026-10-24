/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

func TestLogsTargets_SkipsValidate(t *testing.T) {
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

	got, err := logsTargets(manifestFile, false, 0)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(home, ".shinyctl", "logs", "g-n.log")
	if len(got) != 1 || got[0].Path != want {
		t.Errorf("logsTargets() = %v, want only %q", got, want)
	}
}

func TestLogsTargets_RequiresGroupAndName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for name, body := range map[string]string{
		"no group": "app-name: n\n",
		"no name":  "app-group: g\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := logsTargets(writeLogsManifest(t, body), false, 0)
			if err == nil || !strings.Contains(err.Error(), "app-group and app-name are required") {
				t.Errorf("logsTargets() error = %v, want it to say app-group and app-name are required", err)
			}
		})
	}
}

func TestLogsTargets_API(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := logsTargets("", true, 0)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(home, ".shinyctl", "api.log")
	if len(got) != 1 || got[0].Path != want {
		t.Errorf("logsTargets(api) = %v, want only %q", got, want)
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

// writeCopyLogs gives an app called n in group g a log for each of its
// copies, with the lines "copy N line K".
func writeCopyLogs(t *testing.T, copies int) (manifestFile string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	manifestFile = writeLogsManifest(t, "app-group: g\napp-name: n\nentrypoint-file: ./app.R\nport: 8881\n")
	dir := filepath.Join(home, ".shinyctl", "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= copies; n++ {
		name := "g-n.log"
		if n > 1 {
			name = fmt.Sprintf("g-n@%d.log", n)
		}
		var body strings.Builder
		fmt.Fprintf(&body, "--- shinyctl: starting copy %d at 2026-10-24T12:00:00Z ---\n", n)
		for k := 1; k <= 3; k++ {
			fmt.Fprintf(&body, "copy %d line %d\n", n, k)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return manifestFile
}

func TestLogs_Copy(t *testing.T) {
	file := writeCopyLogs(t, 2)

	t.Run("one copy", func(t *testing.T) {
		logs, err := logsTargets(file, false, 2)
		if err != nil || len(logs) != 1 || logs[0].Copy != 2 {
			t.Fatalf("logsTargets = %v, %v", logs, err)
		}
		var out bytes.Buffer
		if err := printLogs(t.Context(), &out, logs[0].Path, 100, false, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "copy 1") || !strings.Contains(out.String(), "starting copy 2") || !strings.Contains(out.String(), "copy 2 line 3") {
			t.Errorf("output = %q, want only copy 2", out.String())
		}
	})

	t.Run("a copy with no log", func(t *testing.T) {
		logs, err := logsTargets(file, false, 5)
		if err != nil {
			t.Fatal(err)
		}
		err = printLogs(t.Context(), &bytes.Buffer{}, logs[0].Path, 10, false, time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "no log yet at "+logs[0].Path) {
			t.Errorf("err = %v", err)
		}
	})

	t.Run("every copy, with headers", func(t *testing.T) {
		logs, err := logsTargets(file, false, 0)
		if err != nil || len(logs) != 2 {
			t.Fatalf("logsTargets = %v, %v", logs, err)
		}
		var out bytes.Buffer
		if err := printCopyLogs(t.Context(), &out, logs, 2, false, time.Millisecond); err != nil {
			t.Fatal(err)
		}
		want := "==> copy 1 <==\ncopy 1 line 2\ncopy 1 line 3\n\n==> copy 2 <==\ncopy 2 line 2\ncopy 2 line 3\n"
		if out.String() != want {
			t.Errorf("output =\n%s\nwant\n%s", out.String(), want)
		}
	})

	t.Run("one copy logged is printed as before", func(t *testing.T) {
		single := writeCopyLogs(t, 1)
		logs, err := logsTargets(single, false, 0)
		if err != nil || len(logs) != 1 {
			t.Fatalf("logsTargets = %v, %v", logs, err)
		}
	})

	t.Run("no log at all is copy 1's path", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		logs, err := logsTargets(writeLogsManifest(t, "app-group: g\napp-name: n\n"), false, 0)
		if err != nil || len(logs) != 1 || !strings.HasSuffix(logs[0].Path, "g-n.log") {
			t.Errorf("logsTargets = %v, %v", logs, err)
		}
	})
}

func TestLogs_FollowPrefixesEachCopy(t *testing.T) {
	file := writeCopyLogs(t, 2)
	logs, err := logsTargets(file, false, 0)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- printCopyLogs(ctx, &out, logs, 1, true, 5*time.Millisecond) }()

	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(out.String(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("output = %q, want it to contain %q", out.String(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor("copy 2 line 3\n")

	append1, err := os.OpenFile(logs[0].Path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer append1.Close()
	append2, err := os.OpenFile(logs[1].Path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer append2.Close()

	// A partial line is held until its newline.
	append1.WriteString("half a li")
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(out.String(), "half a li") {
		t.Fatalf("a partial line was printed: %q", out.String())
	}
	append1.WriteString("ne\n")
	append2.WriteString("from two\n")
	waitFor("[copy 1] half a line\n")
	waitFor("[copy 2] from two\n")

	// What is held back when the command ends is shown, not lost.
	append2.WriteString("unfinished")
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("printCopyLogs = %v after cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("printCopyLogs did not return after cancel")
	}
	if !strings.Contains(out.String(), "[copy 2] unfinished\n") {
		t.Errorf("the last partial line was lost: %q", out.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" || strings.HasPrefix(line, "==> ") || strings.HasPrefix(line, "copy ") {
			continue // the header and the tail printed before following
		}
		if !strings.HasPrefix(line, "[copy 1] ") && !strings.HasPrefix(line, "[copy 2] ") {
			t.Errorf("line %q has no copy prefix", line)
		}
	}
}

func TestPrefixWriter(t *testing.T) {
	var out bytes.Buffer
	pw := &prefixWriter{mu: &sync.Mutex{}, out: &out, prefix: "[x] "}
	pw.Write([]byte("one\ntw"))
	if out.String() != "[x] one\n" {
		t.Errorf("output = %q after a partial line", out.String())
	}
	pw.Write([]byte("o\nthree\n"))
	if out.String() != "[x] one\n[x] two\n[x] three\n" {
		t.Errorf("output = %q", out.String())
	}
	pw.Write([]byte("last, no newline"))
	pw.Flush()
	pw.Flush()
	if !strings.HasSuffix(out.String(), "[x] last, no newline\n") || strings.Count(out.String(), "last, no newline") != 1 {
		t.Errorf("output = %q after a flush", out.String())
	}
}

func TestLogsCommand_CopyFlagErrors(t *testing.T) {
	file := writeCopyLogs(t, 2)
	run := func(args ...string) error {
		logsCopy, logsAPI, logsManifestFile = 0, false, ""
		rootCmd.SetArgs(append([]string{"logs"}, args...))
		rootCmd.SetOut(io.Discard)
		rootCmd.SetErr(io.Discard)
		defer func() {
			logsCopy, logsAPI, logsManifestFile = 0, false, ""
			logsCmd.Flags().Set("copy", "0")
			logsCmd.Flags().Lookup("copy").Changed = false
			logsCmd.Flags().Lookup("api").Changed = false
			logsCmd.Flags().Lookup("file").Changed = false
		}()
		return rootCmd.Execute()
	}

	if err := run("--api", "--copy", "2"); err == nil || !strings.Contains(err.Error(), "--copy goes with --file, not --api") {
		t.Errorf("--copy with --api: %v", err)
	}
	if err := run("--file", file, "--copy", "-1"); err == nil || !strings.Contains(err.Error(), "--copy must be 0 (every copy) or more") {
		t.Errorf("--copy -1: %v", err)
	}
}
