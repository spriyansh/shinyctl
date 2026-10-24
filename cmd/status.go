/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"
	"io"
	"math"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/process"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show every app shinyctl-api currently knows about",

	// Deliberately does NOT call process.EnsureAPIRunning like apply does:
	// status is read-only, and a query shouldn't launch a background
	// process as a side effect. If nothing's running, that's useful status
	// to report, not something to paper over.
	RunE: func(cmd *cobra.Command, args []string) error {
		socketPath, err := process.SocketPath()
		if err != nil {
			return err
		}

		if !process.SocketDialable(socketPath) {
			fmt.Println("shinyctl-api is not running — nothing is currently being managed")
			return nil
		}

		resp, err := api.Status(socketPath)
		if err != nil {
			return err
		}

		if len(resp.Apps) == 0 {
			fmt.Println("shinyctl-api is running but knows about no apps yet (run `apply` at least once)")
			return nil
		}

		return writeStatus(os.Stdout, resp.Apps)
	},
}

// writeStatus prints the status table.
func writeStatus(out io.Writer, apps []api.AppStatus) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPORT\tPID\tSTATUS\tREADY\tMEM\tCRASHES\tSTARTED")

	for _, app := range apps {
		status := "dead"
		ready := "-"
		started := "-"
		crashes := "-"

		switch {
		case app.Stopped:
			status = "stopped"
		case app.Alive:
			status = "alive"
			ready = "starting"
			if app.Ready {
				ready = "ready"
			}
		case app.CrashCount > 0:
			status = "crash-looping"
		}

		if app.CrashCount > 0 {
			crashes = fmt.Sprintf("%d", app.CrashCount)
		}

		if !app.StartedAt.IsZero() {
			started = app.StartedAt.Format(time.RFC3339)
		}

		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
			app.Name, app.Port, app.PID, status, ready, formatMem(app.RSSBytes), crashes, started)
	}

	if err := w.Flush(); err != nil {
		return err
	}

	// A running app can still be unreachable: its port belongs to something
	// else. The table can't say so, and the reason is otherwise only in the
	// log.
	for _, app := range apps {
		if app.NotServed != "" {
			fmt.Fprintf(out, "\n%s: its port is not served: %s\n", app.Name, app.NotServed)
		}
	}
	return nil
}

// formatMem renders a byte count in the shortest readable form: K under a
// MiB, whole M under a GiB, then G with one decimal. Powers of 1024. Zero
// means unknown (not running, or not measured) and shows as "-".
func formatMem(bytes int64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)

	switch {
	case bytes <= 0:
		return "-"
	case bytes < mib:
		return fmt.Sprintf("%dK", bytes/kib)
	}

	// Rounding can carry into the next unit (1023.6 MiB), so decide the
	// unit after rounding.
	if m := int64(math.Round(float64(bytes) / mib)); m < 1024 {
		return fmt.Sprintf("%dM", m)
	}
	return fmt.Sprintf("%.1fG", float64(bytes)/gib)
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
