package process

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"syscall"
)

// PortAvailable reports whether host:port is free to bind on this machine
// right now, by actually binding it briefly. Meant to be checked only
// immediately before launching a process that's about to try to bind it
// itself — turns "Rscript exited, go dig through its log to find out
// httpuv couldn't bind" into a clear, immediate error instead. Doesn't
// close the window between this check and the real bind entirely —
// something else could still grab the port in between — it narrows it,
// the same way the PID-reuse guard narrows rather than closes its own gap.
//
// The network is chosen from the address family, never the ambiguous
// "tcp" form: a bare net.Listen("tcp", ":port") can resolve to an IPv6
// wildcard socket on a dual-stack machine, which does *not* collide with
// an existing IPv4-only listener on the same port number. Found live: a
// leftover R process really was holding the port (confirmed via lsof),
// but the "tcp"/"" version of this check reported it as available anyway,
// silently defeating the whole point of the check. Binding exactly what
// Shiny will bind — same family, same address — is what makes the answer
// mean something.
//
// For a specific (non-wildcard) host the answer is best-effort: whether
// binding 127.0.0.1:P succeeds while something holds 0.0.0.0:P depends on
// the OS's SO_REUSEADDR rules (Go sets it on listeners), and macOS and
// Linux disagree. A collision this misses still surfaces as a Start
// failure through the same crash-backoff path, just with a worse message.
func PortAvailable(host string, port int) error {
	ln, err := net.Listen(tcpNetwork(host), net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("%s is not available (%w) — pick a different port, or check what else is using it", net.JoinHostPort(host, strconv.Itoa(port)), err)
	}
	return ln.Close()
}

// PortAvailableAlongside is PortAvailable for a port the load balancer's own
// listener on the same app already holds: it binds with SO_REUSEPORT, as the
// load balancer's sockets do, so that socket doesn't count against the
// address while another program's does. It is what a move of an app's host
// on the same port is checked with.
func PortAvailableAlongside(host string, port int) error {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(context.Background(), tcpNetwork(host), net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("%s is not available (%w) — pick a different port, or check what else is using it", net.JoinHostPort(host, strconv.Itoa(port)), err)
	}
	return ln.Close()
}

// tcpNetwork picks "tcp4" or "tcp6" for host, so a listen or dial on it
// stays in the same address family Shiny binds. host is already validated
// as an IP literal by manifest.Validate; anything else falls back to IPv4.
func tcpNetwork(host string) string {
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "tcp6"
	}
	return "tcp4"
}
