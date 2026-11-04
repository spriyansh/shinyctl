/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/registry"
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

		out := cmd.OutOrStdout()
		now := statusNow()

		if !process.SocketDialable(socketPath) {
			registryPath, err := process.RegistryPath()
			if err != nil {
				return err
			}
			if statusAsJSON {
				doc, err := notRunningJSON(registryPath, now)
				if err != nil {
					return err
				}
				return writeStatusJSON(out, doc)
			}
			fmt.Fprintln(out, notRunningLine(registryPath))
			return nil
		}

		resp, err := api.Status(socketPath)
		if err != nil {
			return err
		}

		if statusAsJSON {
			return writeStatusJSON(out, statusJSON(resp, now))
		}

		if len(resp.Apps) == 0 {
			fmt.Fprintln(out, "shinyctl-api is running but knows about no apps yet (run `apply` at least once)")
			return nil
		}

		return writeStatus(out, resp)
	},
}

// writeStatus prints the status table, a row per copy, and what the table
// can't say under it.
func writeStatus(out io.Writer, resp api.StatusResponse) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tCOPY\tROLE\tPORT\tPID\tSTATUS\tREADY\tMEM\tSESSIONS\tCRASHES\tSTARTED")

	for _, app := range resp.Apps {
		if len(app.Copies) == 0 {
			fmt.Fprintf(w, "%s\t-\t-\t%d\t0\t%s\t-\t-\t-\t-\t-\n", app.Name, app.Port, statusWord(app, api.CopyStatus{}))
			continue
		}

		for _, c := range app.Copies {
			status := statusWord(app, c)
			ready := readyWord(status, c)
			if ready == "" {
				ready = "-"
			}
			started := "-"
			crashes := "-"
			role := "-"
			sessions := "-"

			if n := crashesShown(app, c); n > 0 {
				crashes = fmt.Sprintf("%d", n)
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
				app.Name, number, role, app.Port, c.PID, status, ready, formatMem(c.MemoryBytes), sessions, crashes, started)
		}
	}

	if err := w.Flush(); err != nil {
		return err
	}

	for _, line := range topNotes(resp) {
		fmt.Fprintf(out, "\n%s\n", line)
	}

	// Kind by kind across the apps, so the notes about one thing read
	// together.
	now := statusNow()
	for _, kind := range appNoteKinds {
		for _, app := range resp.Apps {
			for _, line := range kind(app, now) {
				fmt.Fprintf(out, "\n%s: %s\n", app.Name, line)
			}
		}
	}
	return nil
}

// portInUse says the app's port is held by something else: no copy was
// started, and none of this is a crash.
func portInUse(app api.AppStatus) bool {
	return app.NotStarted != "" && !slices.ContainsFunc(app.Copies, func(c api.CopyStatus) bool { return c.Alive })
}

// statusWord is the STATUS column: stopped, port-in-use, alive, exited,
// crash-looping or dead, in that order. With a zero CopyStatus it is the row
// of an app that has no copies.
func statusWord(app api.AppStatus, c api.CopyStatus) string {
	switch {
	case app.Stopped:
		return "stopped"
	case portInUse(app):
		return "port-in-use"
	case c.Alive:
		return "alive"
	case c.GaveUp:
		// The process is gone and the restart policy leaves it so.
		return "exited"
	case c.CrashCount > 0:
		return "crash-looping"
	}
	return "dead"
}

// readyWord is the READY column: ready, page-failing or starting for an alive
// copy, "" otherwise.
func readyWord(status string, c api.CopyStatus) string {
	if status != "alive" {
		return ""
	}
	switch {
	case c.Ready:
		return "ready"
	case c.PageProblem != "":
		return "page-failing"
	}
	return "starting"
}

// crashesShown is the CRASHES column as a number: the copy's crashes, none
// while the port is in use.
func crashesShown(app api.AppStatus, c api.CopyStatus) int {
	if c.CrashCount > 0 && !portInUse(app) {
		return c.CrashCount
	}
	return 0
}

// balancerStartingNote is the line under the table while the load balancer
// starts.
const balancerStartingNote = "The load balancer is starting or being replaced: copies show as starting until it is done, usually in under a second."

// topNotes are the lines under the table that name no app: memory, the
// machine, the load balancer starting.
func topNotes(resp api.StatusResponse) []string {
	var lines []string
	if line := memoryLine(resp); line != "" {
		lines = append(lines, line)
	}
	if line := machineLine(resp); line != "" {
		lines = append(lines, line)
	}
	if resp.BalancerStarting {
		lines = append(lines, balancerStartingNote)
	}
	return lines
}

// appNoteKinds are the lines under the table about one app, kind by kind in
// the order status prints them, each as printed after "<name>: ": not served,
// not started, rollout, exited, page, memory, and last one kind spareAndDrain.
// A running app can still be unreachable (its port belongs to something else):
// the table can't say so, and the reason is otherwise only in the log.
var appNoteKinds = []func(app api.AppStatus, now time.Time) []string{
	func(app api.AppStatus, _ time.Time) []string {
		if app.NotServed == "" {
			return nil
		}
		return []string{"its port is not served: " + app.NotServed}
	},
	func(app api.AppStatus, _ time.Time) []string {
		if app.NotStarted == "" {
			return nil
		}
		return []string{"not started: " + app.NotStarted}
	},
	func(app api.AppStatus, now time.Time) []string { return oneNote(rolloutNote(app, now)) },
	func(app api.AppStatus, now time.Time) []string { return oneNote(exitedNote(app, now)) },
	pageNote,
	func(app api.AppStatus, _ time.Time) []string { return oneNote(memoryNote(app)) },
	spareAndDrain,
}

func oneNote(note string) []string {
	if note == "" {
		return nil
	}
	return []string{note}
}

// spareAndDrain is the spare note and then the drain note, each with its full
// stop and each only when there is one. They are one kind, so that one app's
// two lines stay together and the next app's come after.
func spareAndDrain(app api.AppStatus, now time.Time) []string {
	var lines []string
	for _, note := range []string{spareNote(app), drainNote(app, now)} {
		if note != "" {
			lines = append(lines, note+".")
		}
	}
	return lines
}

// appNotes are one app's lines, every kind in turn.
func appNotes(app api.AppStatus, now time.Time) []string {
	var lines []string
	for _, kind := range appNoteKinds {
		lines = append(lines, kind(app, now)...)
	}
	return lines
}

// statusNow is the clock of the notes; the tests replace it. Only writeStatus
// and the command read it: the notes take the time they are given.
var statusNow = time.Now

func plural(list []int, one, many string) string {
	if len(list) == 1 {
		return one
	}
	return many
}

// rolloutNote explains a redeploy that has not taken over yet: copies of a
// previous version serve (they are not draining) while the new ones are not
// ready. If a new copy is crashing, has given up, or runs with a page that
// fails, the takeover won't come by itself, and the note says so and where to
// look. It is empty when there is nothing of the kind.
func rolloutNote(app api.AppStatus, now time.Time) string {
	var old, waiting, failing, gaveUp, pageFailing []int
	var firstProblem *api.CopyStatus // the lowest copy whose page fails
	var slowest *api.CopyStatus      // the lowest waiting copy whose page has not answered yet
	allSkip := true
	for i, c := range app.Copies {
		switch {
		case c.Outdated && c.Alive && c.Role != "draining":
			old = append(old, c.Copy)
			allSkip = allSkip && c.SkipDrain
		case !c.Outdated && c.Role != "draining" && !(c.Alive && c.Ready):
			waiting = append(waiting, c.Copy)
			if c.Alive && !c.PageWaitingSince.IsZero() && slowest == nil {
				slowest = &app.Copies[i]
			}
			switch {
			case c.Alive && c.PageProblem != "":
				pageFailing = append(pageFailing, c.Copy)
				if firstProblem == nil {
					firstProblem = &app.Copies[i]
				}
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

	if len(failing)+len(gaveUp)+len(pageFailing) > 0 {
		var why string
		switch {
		case len(pageFailing) > 0 && len(failing)+len(gaveUp) == 0:
			problem := fmt.Sprintf("copy %d's page %s (see shinyctl logs --copy %d)", firstProblem.Copy, firstProblem.PageProblem, firstProblem.Copy)
			if len(waiting) == 1 {
				why = problem
			} else {
				why = fmt.Sprintf("%s are not ready; %s", reconcile.ListCopies(waiting), problem)
			}
		case len(pageFailing) > 0:
			bad := slices.Concat(failing, gaveUp, pageFailing)
			slices.Sort(bad)
			why = fmt.Sprintf("%s are not ready (see shinyctl logs --copy %d)", reconcile.ListCopies(bad), bad[0])
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
	starting := reconcile.ListCopies(waiting)
	if slowest != nil {
		starting += fmt.Sprintf(" (copy %d's page has not answered after %s)", slowest.Copy,
			reconcile.FormatDuration(now.Sub(slowest.PageWaitingSince).Round(time.Second)))
	}
	return fmt.Sprintf("new version starting in %s; %s %s serving the previous version until it is ready, then %s",
		starting, reconcile.ListCopies(old), plural(old, "keeps", "keep"), then)
}

// exitedNote says why copies are down that the restart policy leaves down,
// and what brings them back. A copy that a note about a redeploy already
// names is left to that note. It is empty when no copy has exited that way.
func exitedNote(app api.AppStatus, now time.Time) string {
	if rolloutNote(app, now) != "" {
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

// pageNote says which copies are listening but are not ready for want of
// their page, and, when that leaves the app with no copy serving, what a
// visitor gets meanwhile. A copy that a note about a redeploy already names
// is left to that note. It is empty when there is nothing of the kind.
func pageNote(app api.AppStatus, now time.Time) []string {
	if rolloutNote(app, now) != "" {
		return nil
	}
	var lines []string
	for _, c := range app.Copies {
		if !c.Alive || c.Ready {
			continue
		}
		switch {
		case c.PageProblem != "":
			lines = append(lines, fmt.Sprintf("copy %d is not ready: its page %s (see shinyctl logs --copy %d)", c.Copy, c.PageProblem, c.Copy))
		case !c.PageWaitingSince.IsZero():
			lines = append(lines, fmt.Sprintf("copy %d is not ready: its page has not answered after %s (see shinyctl logs --copy %d)", c.Copy,
				reconcile.FormatDuration(now.Sub(c.PageWaitingSince).Round(time.Second)), c.Copy))
		}
	}
	if len(lines) > 0 && !slices.ContainsFunc(app.Copies, func(c api.CopyStatus) bool { return c.Role != "" }) {
		lines = append(lines, fmt.Sprintf("no copy serves yet: a visitor waits up to %s, then gets a page saying the app is restarting, until a copy's page answers",
			reconcile.FormatDuration(proxy.DefaultTryDuration)))
	}
	return lines
}

// memoryNote says that copies were killed for going over max-memory, while
// the kill is what their crash count shows: that they start again after their
// backoff, or are back, or (when the restart policy ends their story, which
// exitedNote explains) that they stay down. It is not hidden by a rollout, as
// exitedNote is: a redeploy does not say why a copy died. The size named is the
// app's limit now. It is the one the copy was killed over, because an apply or
// restart clears the exit history, and a live change of the limit does not.
// It is empty when there is nothing of the kind.
func memoryNote(app api.AppStatus) string {
	var waiting, back, gaveUp []int
	for _, c := range app.Copies {
		if c.LastExitReason != string(process.ExitOOM) || (c.CrashCount == 0 && !c.GaveUp) {
			continue
		}
		switch {
		case c.Alive:
			back = append(back, c.Copy)
		case c.GaveUp:
			gaveUp = append(gaveUp, c.Copy)
		default:
			waiting = append(waiting, c.Copy)
		}
	}

	over := "going over max-memory"
	if app.MaxMemoryBytes > 0 {
		over += fmt.Sprintf(" (%s)", formatMem(app.MaxMemoryBytes))
	}
	was := func(list []int) string {
		return fmt.Sprintf("%s %s killed for %s", reconcile.ListCopies(list), plural(list, "was", "were"), over)
	}

	var sentences []string
	if len(waiting) > 0 {
		sentences = append(sentences, fmt.Sprintf("%s; %s again after %s backoff.", was(waiting), plural(waiting, "it starts", "they start"), plural(waiting, "its", "their")))
	}
	if len(back) > 0 {
		sentences = append(sentences, fmt.Sprintf("%s and %s back.", was(back), plural(back, "is", "are")))
	}
	if len(gaveUp) > 0 {
		sentences = append(sentences, was(gaveUp)+".")
	}
	return strings.Join(sentences, " ")
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
		case c.Alive && !c.Ready && !c.Outdated && c.Role == "" && c.PageProblem == "":
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
			total += c.MemoryBytes
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
	if resp.BalancerMemory > 0 {
		line += fmt.Sprintf(", plus %s for the load balancer", formatMem(resp.BalancerMemory))
	}
	return line + "."
}

// machineLine says the machine is low on memory, as one paragraph with a
// way out. It is empty when the machine is healthy, or when shinyctl-api sent
// no reading (an older build, or none could be read). The verdict is the
// daemon's.
func machineLine(resp api.StatusResponse) string {
	m := resp.MachineMemory
	if m == nil || !m.Low {
		return ""
	}
	said := describeMachine(*m)
	line := strings.ToUpper(said[:1]) + said[1:] + "."

	// The advice looks at what is running, not at what was measured: copies
	// that are alive but measured at 0 bytes still hold memory.
	var running, unlimited, warm bool
	for _, app := range resp.Apps {
		for _, c := range app.Copies {
			if !c.Alive {
				continue
			}
			running = true
			unlimited = unlimited || app.MaxMemoryBytes == 0
			warm = warm || c.Role == "warm"
		}
	}
	switch {
	case !running:
		return line + " None of shinyctl's apps is running; other programs hold the memory."
	case unlimited:
		return line + " Stop an app, or set max-memory so a copy that grows is killed before the machine is out of memory."
	case warm:
		return line + " Stop an app, or lower warm-replicas."
	}
	return line + " Stop an app."
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
	statusCmd.Flags().BoolVar(&statusAsJSON, "json", false, "print what status knows as one JSON document, for scripts and monitoring")
}

// notRunningLine is what status says while shinyctl-api is not running.
// Nothing is watched then, but the apps it was managing are still
// registered (shutdown keeps them, stop and delete do not), and they start
// again with the next shinyctl-api. The line names them so a status after
// a shutdown does not read as if they were gone. It only reads.
func notRunningLine(registryPath string) string {
	const prefix = "shinyctl-api is not running"
	reg, err := registry.Open(registryPath)
	if err != nil {
		return prefix
	}
	var names []string
	for _, spec := range reg.All() {
		if reg.Managed(spec.AppStateFile) {
			names = append(names, spec.AppName)
		}
	}
	if len(names) == 0 {
		return prefix + " and no app is registered"
	}
	sort.Strings(names)
	verb := "starts"
	if len(names) > 1 {
		verb = "start"
	}
	return fmt.Sprintf("%s; %s %s again with shinyctl serve -d or any apply", prefix, listNames(names), verb)
}
