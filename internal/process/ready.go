package process

import (
	"net"
	"strconv"
	"time"
)

// Listening reports whether something accepts TCP connections on host:port:
// the port half of a copy's readiness. It says nothing about the page. A
// Shiny app is alive for a while before it listens (it may load a large
// dataset at startup), and it listens before anything has asked for its page.
//
// Dials the address the app actually bound, not a fixed 127.0.0.1: an app
// bound to one specific interface isn't listening on loopback at all, and
// would otherwise show as "starting" forever.
func Listening(host string, port int) bool {
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
