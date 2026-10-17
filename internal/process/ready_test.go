package process

import (
	"net"
	"testing"
)

func TestIsReady(t *testing.T) {
	t.Run("wildcard-bound listener is ready via loopback", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		if !IsReady("0.0.0.0", port) {
			t.Errorf("IsReady(0.0.0.0, %d) = false for a live listener", port)
		}
	})

	t.Run("loopback-bound listener is ready on loopback", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		if !IsReady("127.0.0.1", port) {
			t.Errorf("IsReady(127.0.0.1, %d) = false for a live listener", port)
		}
	})

	t.Run("nothing listening is not ready", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()

		if IsReady("127.0.0.1", port) {
			t.Errorf("IsReady(127.0.0.1, %d) = true with nothing listening", port)
		}
	})

	t.Run("IPv6 wildcard is dialed on IPv6 loopback", func(t *testing.T) {
		ln, err := net.Listen("tcp6", "[::]:0")
		if err != nil {
			t.Skip("no IPv6 on this machine")
		}
		defer ln.Close()
		port := ln.Addr().(*net.TCPAddr).Port

		if !IsReady("::", port) {
			t.Errorf("IsReady(::, %d) = false for a live listener", port)
		}
	})
}

func TestDialHost(t *testing.T) {
	for host, want := range map[string]string{
		"0.0.0.0":     "127.0.0.1",
		"::":          "::1",
		"127.0.0.1":   "127.0.0.1",
		"192.168.1.5": "192.168.1.5",
		"::1":         "::1",
	} {
		if got := dialHost(host); got != want {
			t.Errorf("dialHost(%q) = %q, want %q", host, got, want)
		}
	}
}
