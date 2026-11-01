// Package lb is the copies table of shinyctl's load balancer: which copies
// of each app exist, by role, kept in the load balancer's own memory and
// changed through a control-socket route. The upstream source and the
// sticky selection policy read it for every request, so a copy that dies, a
// spare that takes over or a scale-up changes where visitors go without a
// reload of the settings. A reload rebinds every listener and drops the
// connections that arrive while it runs; a table update touches none.
//
// The table and the set of active connections (active) are package globals:
// the engine builds new module instances on every reload, and they must
// outlive them. Tests in this package and in internal/proxy share them, so
// they must never call t.Parallel, and they reset the table with Set(nil)
// when they are done.
package lb

import (
	"github.com/caddyserver/caddy/v2"
)

func init() {
	caddy.RegisterModule(Copies{})
	caddy.RegisterModule(&Sticky{})
	caddy.RegisterModule(adminCopies{})
	caddy.RegisterModule(InFlightCounter{})
}
