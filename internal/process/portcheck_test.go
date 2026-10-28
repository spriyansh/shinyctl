package process

import (
	"context"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPortAvailable(t *testing.T) {
	t.Run("free port is available", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()

		if err := PortAvailable("0.0.0.0", port); err != nil {
			t.Errorf("PortAvailable(0.0.0.0, %d) = %v, want nil", port, err)
		}
	})

	t.Run("occupied port is reported unavailable", func(t *testing.T) {
		// tcp4 on 0.0.0.0, matching exactly how process.Start launches
		// Shiny by default — regression test for a real bug found live: a
		// bare "tcp"/":0" listener here can land on an IPv6 wildcard
		// socket on a dual-stack machine, which doesn't collide with an
		// IPv4-only occupant and would make this test pass for the wrong
		// reason while PortAvailable stayed silently broken.
		ln, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		if err := PortAvailable("0.0.0.0", port); err == nil {
			t.Errorf("PortAvailable(0.0.0.0, %d) = nil, want an error — port is held open", port)
		}
	})

	t.Run("occupied loopback port is reported unavailable", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		if err := PortAvailable("127.0.0.1", port); err == nil {
			t.Errorf("PortAvailable(127.0.0.1, %d) = nil, want an error — port is held open", port)
		}
	})

	t.Run("IPv6 host is checked in its own family", func(t *testing.T) {
		ln, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			t.Skip("no IPv6 loopback on this machine")
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		if err := PortAvailable("::1", port); err == nil {
			t.Errorf("PortAvailable(::1, %d) = nil, want an error — port is held open", port)
		}
	})
}

func TestTCPNetwork(t *testing.T) {
	for host, want := range map[string]string{
		"0.0.0.0":   "tcp4",
		"127.0.0.1": "tcp4",
		"::":        "tcp6",
		"::1":       "tcp6",
		"fe80::1":   "tcp6",
	} {
		if got := tcpNetwork(host); got != want {
			t.Errorf("tcpNetwork(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestPortAvailableAlongside(t *testing.T) {
	reusing := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}); err != nil {
			return err
		}
		return serr
	}}

	t.Run("a listener that shares the port does not count", func(t *testing.T) {
		ln, err := reusing.Listen(context.Background(), "tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		if err := PortAvailableAlongside("127.0.0.1", port); err != nil {
			t.Errorf("PortAvailableAlongside = %v, want nil next to a listener that shares the port", err)
		}
	})

	t.Run("another program's listener does", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		err = PortAvailableAlongside("127.0.0.1", port)
		if err == nil || !strings.Contains(err.Error(), "is not available") {
			t.Errorf("PortAvailableAlongside = %v, want \"is not available\"", err)
		}
	})

	t.Run("a free port is available", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if err := PortAvailableAlongside("127.0.0.1", port); err != nil {
			t.Errorf("PortAvailableAlongside = %v on a free port", err)
		}
	})
}

// TestPortAvailable_MessageBracketsIPv6: the message names the address the
// way the bind error does, "[::1]:8080", not "::1:8080".
func TestPortAvailable_MessageBracketsIPv6(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	for name, check := range map[string]func(string, int) error{"PortAvailable": PortAvailable, "PortAvailableAlongside": PortAvailableAlongside} {
		err := check("::1", port)
		if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("[::1]:%d is not available", port)) {
			t.Errorf("%s = %v, want a message starting with [::1]:%d", name, err, port)
		}
	}
}
