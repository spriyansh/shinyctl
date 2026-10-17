package process

import (
	"net"
	"testing"
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
