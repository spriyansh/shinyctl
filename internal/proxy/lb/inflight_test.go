package lb

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestInFlight_CountsRequestsButNotUpgrades(t *testing.T) {
	var during int
	next := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		during = InFlight()
		return nil
	})

	before := InFlight()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := (InFlightCounter{}).ServeHTTP(httptest.NewRecorder(), r, next); err != nil {
		t.Fatal(err)
	}
	if during != before+1 {
		t.Errorf("in flight during a request = %d, want %d", during, before+1)
	}
	if got := InFlight(); got != before {
		t.Errorf("in flight after it = %d, want %d: the count must go back", got, before)
	}

	// Only a WebSocket upgrade is left out; everything else is counted.
	for _, c := range []struct {
		name        string
		conn, upg   []string
		wantCounted bool
	}{
		{"a WebSocket", []string{"Upgrade"}, []string{"websocket"}, false},
		{"a WebSocket, other case", []string{"upgrade"}, []string{"WebSocket"}, false},
		{"a WebSocket, with Connection a list", []string{"keep-alive, Upgrade"}, []string{"websocket"}, false},
		{"a WebSocket, Connection on a line of its own", []string{"keep-alive", "upgrade"}, []string{"websocket"}, false},
		{"a WebSocket among other protocols", []string{"Upgrade"}, []string{"h2c, websocket"}, false},
		{"h2c, as curl --http2 asks", []string{"Upgrade, HTTP2-Settings"}, []string{"h2c"}, true},
		{"an Upgrade header and no Connection: upgrade", nil, []string{"websocket"}, true},
		{"Connection: upgrade and no Upgrade header", []string{"upgrade"}, nil, true},
		{"Connection: keep-alive with websocket", []string{"keep-alive"}, []string{"websocket"}, true},
		{"near misses: upgraded and websockets", []string{"upgraded"}, []string{"websockets"}, true},
		{"near miss: upgraded with websocket", []string{"upgraded"}, []string{"websocket"}, true},
		{"near miss: upgrade with websockets", []string{"upgrade"}, []string{"websockets"}, true},
		{"near miss: tokens inside a longer one", []string{"x-upgrade"}, []string{"my-websocket"}, true},
		{"nothing", nil, nil, true},
	} {
		r = httptest.NewRequest(http.MethodGet, "/", nil)
		for _, v := range c.conn {
			r.Header.Add("Connection", v)
		}
		for _, v := range c.upg {
			r.Header.Add("Upgrade", v)
		}
		during = -1
		if err := (InFlightCounter{}).ServeHTTP(httptest.NewRecorder(), r, next); err != nil {
			t.Fatal(err)
		}
		want := before
		if c.wantCounted {
			want = before + 1
		}
		if during != want {
			t.Errorf("%s: in flight during it = %d, want %d (counted: %v)", c.name, during, want, c.wantCounted)
		}
	}

	// A panic through the handler still gives the request back.
	func() {
		defer func() { recover() }()
		panics := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error { panic("boom") })
		(InFlightCounter{}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), panics)
	}()
	if got := InFlight(); got != before {
		t.Errorf("in flight after a panic = %d, want %d: the count must go back", got, before)
	}

	// A handler that fails still gives the request back.
	boom := caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error { return caddyhttp.Error(502, nil) })
	(InFlightCounter{}).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil), boom)
	if got := InFlight(); got != before {
		t.Errorf("in flight after a failed request = %d, want %d", got, before)
	}
}
