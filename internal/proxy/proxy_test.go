package proxy

import (
	"log"
	"os"
	"testing"
)

// TestStdLogRestored: the engine redirects the standard log at init, and
// Validate provisions its logging again; neither may leave api.log in JSON.
func TestStdLogRestored(t *testing.T) {
	check := func(when string) {
		t.Helper()
		if log.Writer() != os.Stderr {
			t.Errorf("%s: log writer = %v, want os.Stderr", when, log.Writer())
		}
		if log.Flags() != log.LstdFlags {
			t.Errorf("%s: log flags = %d, want %d", when, log.Flags(), log.LstdFlags)
		}
		if log.Prefix() != "" {
			t.Errorf("%s: log prefix = %q, want empty", when, log.Prefix())
		}
	}
	check("at start")

	cfg, err := Render(testSettings, testApps)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	check("after Validate")
}
