/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"
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

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tPORT\tPID\tSTATUS\tREADY\tCRASHES\tSTARTED")

		for _, app := range resp.Apps {
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

			fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
				app.Name, app.Port, app.PID, status, ready, crashes, started)
		}

		return w.Flush()
	},
}

func init() {
	rootCmd.AddCommand(statusCmd)
}
