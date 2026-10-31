package reconcile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
)

// NothingToChange is Describe's answer when a plan does nothing.
const NothingToChange = "Nothing to change"

// Describe is the reply to apply and restart, and the line the daemon logs
// when it starts something. before is what was observed, p the plan, and
// settings the manifest fields that changed and take effect without a
// restart (see SettingsChanged).
func Describe(spec manifest.AppSpec, before []CopyView, p Plan, settings []string) string {
	var upgrade bool
	var fresh, again, stopped, drainEnded, backoffs []int
	var oldPID int
	known := map[int]CopyView{}
	for _, v := range before {
		known[v.App.Copy] = v
	}
	for _, s := range p.Steps {
		wasDraining := !known[s.Copy].App.DrainingSince.IsZero()
		switch {
		case s.Type == ActionStop && s.Copy == 0:
			oldPID = s.PID
		case s.Type == ActionStop && s.Why == StopGone:
			// a record with no process: nothing to say
		case s.Type == ActionStop && (wasDraining || s.Why == StopTooMany):
			drainEnded = append(drainEnded, s.Copy)
		case s.Type == ActionStop:
			stopped = append(stopped, s.Copy)
		case s.Type == ActionBackoff:
			backoffs = append(backoffs, s.Copy)
		case s.Upgrade:
			upgrade = true
		default:
			if _, ok := known[s.Copy]; ok {
				again = append(again, s.Copy)
			} else {
				fresh = append(fresh, s.Copy)
			}
		}
	}

	name, port := spec.AppName, spec.Port
	timeout := FormatDuration(spec.EffectiveDrainTimeout())
	multi := spec.CopyCount() > 1
	summary := copiesSummary(spec.EffectiveReplicas(), spec.EffectiveWarm())
	fields := func(extra []string) string {
		return strings.Join(OrderFields(dedupe(slices.Concat(extra, settings))), ", ")
	}
	verb := func(l []int, one, many string) string {
		if len(l) == 1 {
			return one
		}
		return many
	}
	// tail is what follows a list of things the plan does besides its main
	// point, each part starting with "; ".
	tail := func() string {
		var s string
		if len(stopped) > 0 {
			s += "; stopping " + ListCopies(stopped)
		}
		if len(p.Drain) > 0 && !p.Switched {
			s += "; draining " + ListCopies(p.Drain)
		}
		if len(drainEnded) > 0 {
			s += "; stopping draining " + ListCopies(drainEnded)
		}
		return s
	}

	switch {
	case upgrade:
		var parts []string
		if multi {
			parts = append(parts, summary)
		}
		if len(settings) > 0 {
			parts = append(parts, "manifest changed: "+fields(nil))
		}
		parts = append(parts, "moved behind the load balancer", fmt.Sprintf("stopped the previous instance: pid %d", oldPID))
		return fmt.Sprintf("Starting app %q on port %d (%s)", name, port, strings.Join(parts, "; "))

	case len(fresh) > 0 && p.Rolling:
		msg := fmt.Sprintf("Restarting app %q", name)
		if f := fields(p.Changed); f != "" {
			msg += " (manifest changed: " + f + ")"
		}
		then := verb(p.Outdated, "drains", "drain") + " for up to " + timeout
		if p.OldStop {
			then = verb(p.Outdated, "stops", "stop")
		}
		msg += fmt.Sprintf(": starting %s; %s %s serving until the new %s ready, then %s",
			ListCopies(fresh), ListCopies(p.Outdated), verb(p.Outdated, "keeps", "keep"), verb(fresh, "one is", "ones are"), then)
		return msg + tail()

	case len(fresh)+len(again) > 0 && countAlive(before) == 0 && len(stopped)+len(drainEnded)+len(p.Drain) == 0:
		var parts []string
		if multi {
			parts = append(parts, summary)
		}
		if len(settings) > 0 {
			parts = append(parts, "manifest changed: "+fields(nil))
		}
		msg := fmt.Sprintf("Starting app %q on port %d", name, port)
		if len(parts) > 0 {
			msg += " (" + strings.Join(parts, "; ") + ")"
		}
		return msg

	case p.Switched:
		serving := slices.Sorted(slices.Values(slices.Concat(p.Active, p.Warm)))
		msg := fmt.Sprintf("Restarted app %q: %s %s the new version", name, ListCopies(serving), verb(serving, "serves", "serve"))
		if len(p.Drain) > 0 {
			msg += fmt.Sprintf("; %s %s for up to %s", ListCopies(p.Drain), verb(p.Drain, "drains", "drain"), timeout)
		}
		if len(fresh) > 0 {
			msg += "; starting " + ListCopies(fresh)
		}
		return msg + tail()

	case p.Rolling && len(fresh)+len(again)+len(stopped)+len(drainEnded)+len(p.Drain) == 0 && len(backoffs) == 0:
		msg := fmt.Sprintf("Restarting app %q", name)
		if len(settings) > 0 {
			msg += " (manifest changed: " + fields(nil) + ")"
		}
		return msg + fmt.Sprintf(": waiting for %s to be ready; %s %s serving until then",
			ListCopies(p.Waiting), ListCopies(p.Outdated), verb(p.Outdated, "keeps", "keep"))

	case len(fresh)+len(again)+len(stopped)+len(drainEnded)+len(p.Drain) > 0:
		var parts []string
		if len(settings) > 0 {
			parts = append(parts, "manifest changed: "+fields(nil))
		}
		if len(fresh) > 0 {
			parts = append(parts, "starting "+ListCopies(fresh))
		}
		if len(again) > 0 {
			parts = append(parts, "starting "+ListCopies(again)+" again")
		}
		if len(stopped) > 0 {
			parts = append(parts, "stopping "+ListCopies(stopped))
		}
		if len(p.Drain) > 0 {
			parts = append(parts, "draining "+ListCopies(p.Drain))
		}
		if len(drainEnded) > 0 {
			parts = append(parts, "stopping draining "+ListCopies(drainEnded))
		}
		if len(settings) > 0 {
			return fmt.Sprintf("Updated %q without a restart (%s)", name, strings.Join(parts, "; "))
		}
		return fmt.Sprintf("Updated %q (%s)", name, strings.Join(parts, "; "))

	case len(backoffs) > 0:
		var lines []string
		for _, n := range backoffs {
			crashes := known[n].App.CrashCount
			last := ""
			if known[n].App.LastExitReason == string(process.ExitOOM) {
				last = ", the last for going over max-memory"
			}
			if multi {
				lines = append(lines, fmt.Sprintf("Backing off restart of %q copy %d after %d crash(es)%s", name, n, crashes, last))
			} else {
				lines = append(lines, fmt.Sprintf("Backing off restart of %q after %d crash(es)%s", name, crashes, last))
			}
		}
		return strings.Join(lines, "; ")

	case len(settings) > 0:
		return fmt.Sprintf("Updated %q without a restart (manifest changed: %s)", name, strings.Join(settings, ", "))
	}
	return NothingToChange
}
