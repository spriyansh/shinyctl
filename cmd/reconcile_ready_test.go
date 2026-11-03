/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/state"
)

// brokenPage is the file whose existence makes the fake Rscript's page answer
// 500 (FAKE_RSCRIPT_PAGE_FAILS_WHILE), one per test; it exists when this
// returns. The spec it is given gets the knob.
func brokenPage(t *testing.T, spec *manifest.AppSpec) (path string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "broken")
	breakPage(t, path)
	spec.Env = map[string]string{"FAKE_RSCRIPT_PAGE_FAILS_WHILE": path}
	return path
}

func breakPage(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixPage(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// pageLine is the line api.log gets once for a copy whose page does what
// problem says, matched by pid since the log is shared by the whole run.
func pageLine(rec state.AppState, problem string) string {
	return fmt.Sprintf("copy %d (pid %d) listens on port %d but its page %s", rec.Copy, rec.PID, rec.Port, problem)
}

// fastRetry makes a failed page be asked again within milliseconds.
func fastRetry() hooks {
	return hooks{PageRetry: 50 * time.Millisecond, PageRetryMax: 200 * time.Millisecond}
}

// failingPage is a PageProbe for a page that answers 500.
func failingPage(context.Context, string, int) process.Page {
	return process.Page{Listening: true, Problem: "answers HTTP 500"}
}

// writeTable writes a copies table the way the gateway does, and returns its
// path.
func writeTable(t *testing.T, table proxy.Table) string {
	t.Helper()
	data, err := json.Marshal(table)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "copies.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTrustListedCopies(t *testing.T) {
	useFakeRscript(t)
	spec := copiesSpec(t, "app", 1, 1)
	cleanupApp(t, spec)
	one, two := adoptCopies(t, spec)
	forget := func() {
		for _, rec := range []state.AppState{one, two} {
			forgetPage(rec)
			ownedPorts.Delete(pidPort{rec.PID, rec.Port})
		}
	}
	forget()
	file := writeTable(t, proxy.Table{spec.AppStateFile: {Active: []string{dialOf(one)}}})

	t.Run("a copy the table lists, started before it was written", func(t *testing.T) {
		forget()
		trustListedCopies(file)
		if !pageReady(one) {
			t.Error("the listed copy's page is not taken as answered")
		}
		if _, ok := ownedPorts.Load(pidPort{one.PID, one.Port}); !ok {
			t.Error("the listed copy's port is not taken as its own")
		}
	})

	t.Run("a running copy the table does not list", func(t *testing.T) {
		forget()
		trustListedCopies(file)
		if pageReady(two) {
			t.Error("a copy that was not listed is trusted")
		}
		if _, ok := ownedPorts.Load(pidPort{two.PID, two.Port}); ok {
			t.Error("a copy that was not listed has its port taken as its own")
		}
	})

	t.Run("a process started after the table was written", func(t *testing.T) {
		forget()
		old := one.StartedAt.Add(-time.Second)
		if err := os.Chtimes(file, old, old); err != nil {
			t.Fatal(err)
		}
		trustListedCopies(file)
		if pageReady(one) {
			t.Error("a process that started after the table was written is trusted")
		}
	})

	t.Run("a file that is not there", func(t *testing.T) {
		forget()
		buf := captureLog(t)
		missing := filepath.Join(t.TempDir(), "copies.json")
		trustListedCopies(missing)
		if pageReady(one) || pageReady(two) {
			t.Error("a copy is trusted without a table")
		}
		if strings.Contains(buf.String(), missing) {
			t.Errorf("a missing table was logged:\n%s", buf.String())
		}
	})

	t.Run("a file that does not parse", func(t *testing.T) {
		forget()
		buf := captureLog(t)
		bad := filepath.Join(t.TempDir(), "copies.json")
		if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		trustListedCopies(bad)
		if pageReady(one) || pageReady(two) {
			t.Error("a copy is trusted from an unusable table")
		}
		if n := strings.Count(buf.String(), bad); n != 1 {
			t.Errorf("the unusable table was logged %d times, want once:\n%s", n, buf.String())
		}
	})
}
