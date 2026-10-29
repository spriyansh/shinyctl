package proxy

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestMain isolates the package from the real home directory, and lets the
// test binary stand in for `shinyctl proxy --config <file>`: the subprocess
// tests start it that way, the same as shinyctl-api starts the real one.
//
// None of the tests in this package may call t.Parallel: the in-process ones
// share the load balancer library's global state.
func TestMain(m *testing.M) {
	if len(os.Args) >= 4 && os.Args[1] == "proxy" && os.Args[2] == "--config" {
		after := 0
		if len(os.Args) >= 6 && os.Args[4] == "--after" {
			after, _ = strconv.Atoi(os.Args[5])
		}
		// Timings a test of the handover changes in the process it starts.
		for env, dst := range map[string]*time.Duration{
			"SHINYCTL_TEST_HANDOVER_GRACE": &handoverGrace,
			"SHINYCTL_TEST_TAKEOVER_WAIT":  &takeoverWait,
			"SHINYCTL_TEST_SERVE_DELAY":    &serveDelay,
			"SHINYCTL_TEST_STOP_TIMEOUT":   &stopTimeout,
			"SHINYCTL_TEST_STOP_DELAY":     &stopDelay,
		} {
			if v := os.Getenv(env); v != "" {
				if d, err := time.ParseDuration(v); err == nil {
					*dst = d
				}
			}
		}
		if err := Run(os.Args[3], after); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// `validate <file>` stands in for shinyctl-api checking settings: it
	// validates the file and exits, so a test can read what it wrote to
	// standard error.
	if len(os.Args) == 3 && os.Args[1] == "validate" {
		cfg, err := os.ReadFile(os.Args[2])
		if err == nil {
			err = Validate(cfg)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	home, err := os.MkdirTemp("", "pt")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, k := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME"} {
		os.Setenv(k, home)
	}

	// Load balancers the supervisor tests start run in sessions of their own,
	// so an interrupted run has to end them itself.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		reapTracked()
		os.RemoveAll(home)
		os.Exit(130)
	}()

	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
