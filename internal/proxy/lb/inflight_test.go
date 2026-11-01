package lb

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// poll waits for cond to hold, for a few seconds.
func poll(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: not true after 5s", what)
}

func TestActiveConns_Transitions(t *testing.T) {
	var a activeConns
	c1, c1p := net.Pipe()
	c2, c2p := net.Pipe()
	defer c1.Close()
	defer c1p.Close()
	defer c2.Close()
	defer c2p.Close()

	check := func(step string, want int) {
		t.Helper()
		if got := a.count(); got != want {
			t.Errorf("%s: %d active, want %d", step, got, want)
		}
	}
	check("at first", 0)
	a.track(c1, http.StateNew)
	check("a new connection", 0)
	a.track(c1, http.StateActive)
	check("a request read", 1)
	a.track(c1, http.StateActive)
	check("a second request on it", 1)
	a.track(c2, http.StateActive)
	check("another connection", 2)
	a.track(c1, http.StateIdle)
	check("the first idle", 1)
	a.track(c1, http.StateIdle)
	check("idle twice", 1)
	a.track(c2, http.StateHijacked)
	check("the second hijacked", 0)
	a.track(c1, http.StateActive)
	a.track(c1, http.StateClosed)
	check("closed while active", 0)
}

func TestInFlight_FollowsTheConnection(t *testing.T) {
	// Each case has a server of its own, so closing it ends the case with
	// every connection of it closed.
	serve := func(t *testing.T, h http.HandlerFunc) *httptest.Server {
		ts := httptest.NewUnstartedServer(h)
		ts.Config.ConnState = active.track
		ts.Start()
		return ts
	}
	get := func(t *testing.T, ts *httptest.Server, c *http.Client) {
		t.Helper()
		resp, err := c.Get(ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	t.Run("a plain request", func(t *testing.T) {
		during := -1
		ts := serve(t, func(w http.ResponseWriter, r *http.Request) { during = InFlight() })
		c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		get(t, ts, c)
		ts.Close()
		if during != 1 {
			t.Errorf("in flight during the request = %d, want 1", during)
		}
		if got := InFlight(); got != 0 {
			t.Errorf("in flight after it = %d, want 0", got)
		}
	})

	t.Run("two requests on one connection", func(t *testing.T) {
		var during []int
		ts := serve(t, func(w http.ResponseWriter, r *http.Request) { during = append(during, InFlight()) })
		tr := &http.Transport{}
		c := &http.Client{Transport: tr}
		get(t, ts, c)
		poll(t, "the connection to be idle", func() bool { return InFlight() == 0 })
		get(t, ts, c)
		tr.CloseIdleConnections()
		ts.Close()
		if len(during) != 2 || during[0] != 1 || during[1] != 1 {
			t.Errorf("in flight during the two requests = %v, want [1 1]", during)
		}
		if got := InFlight(); got != 0 {
			t.Errorf("in flight after them = %d, want 0", got)
		}
	})

	t.Run("a hijacked connection", func(t *testing.T) {
		ts := serve(t, func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		})
		conn, err := net.Dial("tcp", ts.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
		// The server closes its end without an answer; the client end stays
		// open, which is what a session looks like to the counter.
		bufio.NewReader(conn).ReadByte()
		poll(t, "the hijacked connection to leave the count", func() bool { return InFlight() == 0 })
		ts.Close()
	})

	t.Run("a panic", func(t *testing.T) {
		ts := serve(t, func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) })
		c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
		if resp, err := c.Get(ts.URL); err == nil {
			resp.Body.Close()
		}
		ts.Close()
		if got := InFlight(); got != 0 {
			t.Errorf("in flight after a panic = %d, want 0", got)
		}
	})
}

func TestInFlightCounter_ProvisionNeedsAServer(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
	defer cancel()
	if err := (InFlightCounter{}).Provision(ctx); err == nil {
		t.Error("provisioned outside a server, want an error")
	}
}

func TestInFlightCounter_ProvisionRefusesHTTP2(t *testing.T) {
	for _, c := range []struct {
		name string
		srv  *caddyhttp.Server
		ok   bool
	}{
		{"h1 only", &caddyhttp.Server{Protocols: []string{"h1"}}, true},
		{"no protocols set", &caddyhttp.Server{}, false},
		{"h1 and h2", &caddyhttp.Server{Protocols: []string{"h1", "h2"}}, false},
		{"h2c", &caddyhttp.Server{Protocols: []string{"h1", "h2c"}}, false},
		{"per-listener protocols", &caddyhttp.Server{Protocols: []string{"h1"}, ListenProtocols: [][]string{{"h1", "h2"}}}, false},
	} {
		base, cancel := caddy.NewContext(caddy.Context{Context: t.Context()})
		ctx := base.WithValue(caddyhttp.ServerCtxKey, c.srv)
		err := (InFlightCounter{}).Provision(ctx)
		cancel()
		if (err == nil) != c.ok {
			t.Errorf("%s: Provision = %v, want an error: %v", c.name, err, !c.ok)
		}
	}
}
