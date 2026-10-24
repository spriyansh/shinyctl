/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/reconcile"
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

		return writeStatus(os.Stdout, resp)
	},
}

// writeStatus prints the status table, a row per copy, and what the table
// can't say under it.
func writeStatus(out io.Writer, resp api.StatusResponse) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tCOPY\tROLE\tPORT\tPID\tSTATUS\tREADY\tMEM\tSESSIONS\tCRASHES\tSTARTED")

	for _, app := range resp.Apps {
		// The port is held by something else: no copy was started, and none
		// of this is a crash.
		portInUse := app.NotStarted != "" && !slices.ContainsFunc(app.Copies, func(c api.CopyStatus) bool { return c.Alive })

		if len(app.Copies) == 0 {
			status := "dead"
			switch {
			case app.Stopped:
				status = "stopped"
			case portInUse:
				status = "port-in-use"
			}
			fmt.Fprintf(w, "%s\t-\t-\t%d\t0\t%s\t-\t-\t-\t-\t-\n", app.Name, app.Port, status)
			continue
		}

		for _, c := range app.Copies {
			status := "dead"
			ready := "-"
			started := "-"
			crashes := "-"
			role := "-"
			sessions := "-"

			switch {
			case app.Stopped:
				status = "stopped"
			case portInUse:
				status = "port-in-use"
			case c.Alive:
				status = "alive"
				ready = "starting"
				if c.Ready {
					ready = "ready"
				}
			case c.CrashCount > 0:
				status = "crash-looping"
			}

			if c.CrashCount > 0 && !portInUse {
				crashes = fmt.Sprintf("%d", c.CrashCount)
			}
			if !c.StartedAt.IsZero() {
				started = c.StartedAt.Format(time.RFC3339)
			}
			if c.Role != "" {
				role = c.Role
			}
			if c.Sessions != nil {
				sessions = fmt.Sprintf("%d", *c.Sessions)
			}
			number := "-"
			if c.Copy >= 1 {
				number = fmt.Sprintf("%d", c.Copy)
			}

			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				app.Name, number, role, app.Port, c.PID, status, ready, formatMem(c.RSSBytes), sessions, crashes, started)
		}
	}

	if err := w.Flush(); err != nil {
		return err
	}

	if line := memoryLine(resp); line != "" {
		fmt.Fprintf(out, "\n%s\n", line)
	}

	// A running app can still be unreachable: its port belongs to something
	// else. The table can't say so, and the reason is otherwise only in the
	// log.
	for _, app := range resp.Apps {
		if app.NotServed != "" {
			fmt.Fprintf(out, "\n%s: its port is not served: %s\n", app.Name, app.NotServed)
		}
	}
	for _, app := range resp.Apps {
		if app.NotStarted != "" {
			fmt.Fprintf(out, "\n%s: not started: %s\n", app.Name, app.NotStarted)
		}
	}
	for _, app := range resp.Apps {
		var old []int
		for _, c := range app.Copies {
			if c.Outdated {
				old = append(old, c.Copy)
			}
		}
		if len(old) == 0 {
			continue
		}
		verb := "runs"
		if len(old) > 1 {
			verb = "run"
		}
		fmt.Fprintf(out, "\n%s: restarting one copy at a time; %s still %s the previous version\n", app.Name, reconcile.ListCopies(old), verb)
	}
	for _, app := range resp.Apps {
		if note := keptOverNote(app); note != "" {
			fmt.Fprintf(out, "\n%s: %s\n", app.Name, note)
		}
	}
	return nil
}

// keptOverNote explains copies that run the previous version and are kept
// above the number wanted: a restart is in progress and the copies that run
// the new version are not ready, so the old ones go on serving. They go when
// the new ones are ready; if a new copy is crashing or has given up, that
// won't happen by itself, and the note says so and where to look. It is empty
// when there is nothing of the kind.
func keptOverNote(app api.AppStatus) string {
	wanted := app.Replicas + app.WarmReplicas
	if len(app.Copies) <= wanted {
		return ""
	}
	var old, waiting, failing, gaveUp []int
	for _, c := range app.Copies {
		switch {
		case c.Outdated && c.Alive && c.Ready:
			old = append(old, c.Copy)
		case !c.Outdated && !(c.Alive && c.Ready):
			waiting = append(waiting, c.Copy)
			switch {
			case c.GaveUp:
				gaveUp = append(gaveUp, c.Copy)
			case !c.Alive && c.CrashCount > 0:
				failing = append(failing, c.Copy)
			}
		}
	}
	if len(old) == 0 || len(waiting) == 0 {
		return ""
	}

	plural := func(list []int, one, many string) string {
		if len(list) == 1 {
			return one
		}
		return many
	}
	if len(failing)+len(gaveUp) > 0 {
		var why string
		switch {
		case len(gaveUp) > 0 && len(failing) == 0:
			why = fmt.Sprintf("%s gave up starting (see shinyctl logs --copy %d; apply again once it is fixed)", reconcile.ListCopies(gaveUp), gaveUp[0])
		case len(failing) > 0 && len(gaveUp) == 0:
			why = fmt.Sprintf("%s %s (see shinyctl logs --copy %d)", reconcile.ListCopies(failing), plural(failing, "does not start", "do not start"), failing[0])
		default:
			bad := slices.Concat(failing, gaveUp)
			slices.Sort(bad)
			why = fmt.Sprintf("%s do not start (see shinyctl logs --copy %d)", reconcile.ListCopies(bad), bad[0])
		}
		return fmt.Sprintf("%s %s serving the previous version: %s", reconcile.ListCopies(old), plural(old, "keeps", "keep"), why)
	}

	copies := "copies"
	if wanted == 1 {
		copies = "copy"
	}
	return fmt.Sprintf("%s %s until %s %s ready (%d %s wanted)", reconcile.ListCopies(old), plural(old, "stays", "stay"), reconcile.ListCopies(waiting), plural(waiting, "is", "are"), wanted, copies)
}

// memoryLine says what the running copies and the load balancer take
// together, since every copy is a whole R process: "Memory: 281M in 2 R
// processes (1 warm), plus 41M for the load balancer." It is empty when
// nothing runs, or nothing could be measured.
func memoryLine(resp api.StatusResponse) string {
	var total int64
	var running, warm int
	for _, app := range resp.Apps {
		for _, c := range app.Copies {
			if !c.Alive {
				continue
			}
			running++
			total += c.RSSBytes
			if c.Role == "warm" {
				warm++
			}
		}
	}
	if running == 0 || total <= 0 {
		return ""
	}

	noun := "processes"
	if running == 1 {
		noun = "process"
	}
	line := fmt.Sprintf("Memory: %s in %d R %s", formatMem(total), running, noun)
	if warm > 0 {
		line += fmt.Sprintf(" (%d warm)", warm)
	}
	if resp.BalancerRSS > 0 {
		line += fmt.Sprintf(", plus %s for the load balancer", formatMem(resp.BalancerRSS))
	}
	return line + "."
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
