package process

import (
	"net"
	"strconv"
	"time"
)

// IsReady reports whether something is actually accepting TCP connections
// on host:port yet — distinct from IsAlive's PID-level check. A Shiny app
// can be alive (the R process exists) for a while before it's actually
// ready to serve, if it's loading a large dataset at startup; this is
// purely a display signal for `status`, never a restart/promotion input.
//
// Dials the address the app actually bound, not a fixed 127.0.0.1: an app
// bound to one specific interface isn't listening on loopback at all, and
// would otherwise show as "starting" forever.
func IsReady(host string, port int) bool {
	conn, err := net.DialTimeout(tcpNetwork(host), net.JoinHostPort(dialHost(host), strconv.Itoa(port)), 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// dialHost maps a wildcard bind address to the loopback address in the
// same family. Connecting *to* 0.0.0.0 happens to work on Linux and macOS
// but isn't something to rely on; a wildcard listener is by definition
// reachable on loopback.
func dialHost(host string) string {
	switch host {
	case "0.0.0.0":
		return "127.0.0.1"
	case "::":
		return "::1"
	}
	return host
}
