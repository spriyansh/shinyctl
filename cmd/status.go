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
			case c.GaveUp:
				// The process is gone and the restart policy leaves it so.
				status = "exited"
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
		if note := rolloutNote(app); note != "" {
			fmt.Fprintf(out, "\n%s: %s\n", app.Name, note)
		}
	}
	for _, app := range resp.Apps {
		if note := exitedNote(app); note != "" {
			fmt.Fprintf(out, "\n%s: %s\n", app.Name, note)
		}
	}
	now := statusNow()
	for _, app := range resp.Apps {
		for _, note := range []string{spareNote(app), drainNote(app, now)} {
			if note != "" {
				fmt.Fprintf(out, "\n%s: %s.\n", app.Name, note)
			}
		}
	}
	return nil
}

// statusNow is the clock of the notes; the tests replace it.
var statusNow = time.Now

func plural(list []int, one, many string) string {
	if len(list) == 1 {
		return one
	}
	return many
}

// rolloutNote explains a redeploy that has not taken over yet: copies of a
// previous version serve (they are not draining) while the new ones are not
// ready. If a new copy is crashing or has given up, the takeover won't come by
// itself, and the note says so and where to look. It is empty when there is
// nothing of the kind.
func rolloutNote(app api.AppStatus) string {
	var old, waiting, failing, gaveUp []int
	allSkip := true
	for _, c := range app.Copies {
		switch {
		case c.Outdated && c.Alive && c.Role != "draining":
			old = append(old, c.Copy)
			allSkip = allSkip && c.SkipDrain
		case !c.Outdated && c.Role != "draining" && !(c.Alive && c.Ready):
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

	then := plural(old, "drains", "drain")
	if allSkip {
		then = plural(old, "stops", "stop")
	}
	return fmt.Sprintf("new version starting in %s; %s %s serving the previous version until it is ready, then %s",
		reconcile.ListCopies(waiting), reconcile.ListCopies(old), plural(old, "keeps", "keep"), then)
}

// exitedNote says why copies are down that the restart policy leaves down,
// and what brings them back. A copy that a note about a redeploy already
// names is left to that note. It is empty when no copy has exited that way.
func exitedNote(app api.AppStatus) string {
	if rolloutNote(app) != "" {
		return ""
	}
	var exited []int
	all := len(app.Copies) > 0
	for _, c := range app.Copies {
		if c.GaveUp && !c.Alive {
			exited = append(exited, c.Copy)
		} else {
			all = false
		}
	}
	if len(exited) == 0 {
		return ""
	}
	var policy string
	switch app.RestartPolicy {
	case "never":
		policy = "restart-policy never leaves"
	case "on-failure":
		policy = "restart-policy on-failure leaves"
	default:
		policy = "the restart policy leaves"
	}
	verb := "it"
	if len(exited) > 1 {
		verb = "them"
	}
	how := "exited"
	if app.RestartPolicy == "on-failure" {
		how = "exited without an error"
	}
	note := fmt.Sprintf("%s %s and %s %s down; apply or restart starts %s again.",
		reconcile.ListCopies(exited), how, policy, verb, verb)
	if all {
		note += " Its port is closed."
	}
	return note
}

// spareNote says that the app has no spare for the seconds between the new
// version taking over and its spare being ready. It is empty otherwise.
func spareNote(app api.AppStatus) string {
	var starting []int
	active, draining := false, false
	for _, c := range app.Copies {
		switch {
		case c.Role == "draining" && c.Alive:
			draining = true
		case c.Role == "active":
			active = true
		case c.Alive && !c.Ready && !c.Outdated && c.Role == "":
			starting = append(starting, c.Copy)
		}
	}
	if !active || !draining || len(starting) == 0 || app.WarmReplicas == 0 {
		return ""
	}
	return fmt.Sprintf("%s, the new spare, %s starting; until %s ready the app has no spare", reconcile.ListCopies(starting), plural(starting, "is", "are"), plural(starting, "it is", "they are"))
}

// drainNote says how many sessions the draining copies still hold and when
// drain-timeout ends them at the latest. It is empty when nothing drains.
func drainNote(app api.AppStatus, now time.Time) string {
	var drain []int
	sessions, known := 0, true
	var latest time.Time
	for _, c := range app.Copies {
		if c.Role != "draining" || !c.Alive {
			continue
		}
		drain = append(drain, c.Copy)
		if c.Sessions == nil {
			known = false
		} else {
			sessions += *c.Sessions
		}
		if c.DrainEndsAt.After(latest) {
			latest = c.DrainEndsAt
		}
	}
	if len(drain) == 0 {
		return ""
	}
	list := reconcile.ListCopies(drain)
	left := latest.Sub(now)
	each := plural(drain, "it", "each")
	switch {
	case known && sessions == 0:
		return fmt.Sprintf("%s %s draining with no sessions; %s within seconds", list, plural(drain, "is", "are"), plural(drain, "it stops", "they stop"))
	case left <= 0:
		with := ""
		if known {
			with = " with " + sessionsText(sessions)
		}
		return fmt.Sprintf("%s %s draining%s; %s within seconds", list, plural(drain, "is", "are"), with, plural(drain, "it stops", "they stop"))
	case !known:
		return fmt.Sprintf("%s %s draining; %s stops when its sessions end, or in %s at the latest", list, plural(drain, "is", "are"), each, formatLeft(left))
	}
	what := sessionsText(sessions)
	ends := "they end"
	if sessions == 1 {
		ends = "it ends"
	}
	if len(drain) > 1 {
		return fmt.Sprintf("%s are draining with %s; each stops when its sessions end, or in %s at the latest", list, what, formatLeft(left))
	}
	return fmt.Sprintf("%s is draining with %s; it stops when %s, or in %s at the latest", list, what, ends, formatLeft(left))
}

func sessionsText(n int) string {
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

// formatLeft writes a time left for a note: seconds under a minute, minutes
// (rounded up) under an hour, then hours and minutes.
func formatLeft(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(math.Ceil(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(math.Ceil(d.Minutes())))
	}
	mins := int(math.Round(d.Minutes()))
	if mins%60 == 0 {
		return fmt.Sprintf("%dh", mins/60)
	}
	return fmt.Sprintf("%dh%02dm", mins/60, mins%60)
}

// memoryLine says what the running copies and the load balancer take
// together, since every copy is a whole R process: "Memory: 281M in 2 R
// processes (1 warm), plus 41M for the load balancer." It is empty when
// nothing runs, or nothing could be measured.
func memoryLine(resp api.StatusResponse) string {
	var total int64
	var running, warm, draining int
	for _, app := range resp.Apps {
		for _, c := range app.Copies {
			if !c.Alive {
				continue
			}
			running++
			total += c.RSSBytes
			switch c.Role {
			case "warm":
				warm++
			case "draining":
				draining++
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
	switch {
	case warm > 0 && draining > 0:
		line += fmt.Sprintf(" (%d warm, %d draining)", warm, draining)
	case warm > 0:
		line += fmt.Sprintf(" (%d warm)", warm)
	case draining > 0:
		line += fmt.Sprintf(" (%d draining)", draining)
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
