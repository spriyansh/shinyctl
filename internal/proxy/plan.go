package proxy

import (
	"fmt"
	"slices"
	"strings"
)

// PortUsedBy is the error for an app whose address overlaps the one holder
// already has.
func PortUsedBy(app, holder App) error {
	return fmt.Errorf("%s is already used by app %q (app-group %q) — pick a different port", app.Addr(), holder.Name, holder.Group)
}

// Plan decides which of apps the load balancer can serve at once. The
// engine rejects a whole config that listens twice on one address, and an
// overlap such as 0.0.0.0:P with 127.0.0.1:P would otherwise bind both
// through SO_REUSEPORT and split the traffic, so the later claimant is left
// out and told why. Apps already served at the same address (loaded) are
// considered first, so a new app never takes a port from a running one;
// within each group the lower Key wins. keep is sorted by Key.
func Plan(apps, loaded []App) (keep []App, left map[string]error) {
	served := func(a App) bool {
		return slices.ContainsFunc(loaded, func(l App) bool { return l.Key == a.Key && l.Addr() == a.Addr() })
	}

	ordered := slices.Clone(apps)
	slices.SortFunc(ordered, func(a, b App) int {
		if sa, sb := served(a), served(b); sa != sb {
			if sa {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})

	left = map[string]error{}
	for _, a := range ordered {
		i := slices.IndexFunc(keep, a.Overlaps)
		if i >= 0 {
			left[a.Key] = PortUsedBy(a, keep[i])
			continue
		}
		keep = append(keep, a)
	}

	slices.SortFunc(keep, func(a, b App) int { return strings.Compare(a.Key, b.Key) })
	return keep, left
}
