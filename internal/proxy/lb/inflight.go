package lb

import (
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// inFlight counts the requests the load balancer is answering, WebSocket
// upgrades left out: a session never finishes, and is meant to end when the
// load balancer stops. Every other request is counted, whatever headers it
// has (an h2c upgrade offer, say, is answered as plain HTTP/1.1).
var inFlight atomic.Int64

// InFlight is the number of requests being answered now, WebSockets not
// counted. A load balancer that is stopping waits for it to reach zero: the
// engine closes its listeners at once on a stop, but answers the requests in
// flight in goroutines that nothing waits for before the process exits.
func InFlight() int { return int(inFlight.Load()) }

// InFlightCounter is the handler that counts: the first one of every route.
type InFlightCounter struct{}

func (InFlightCounter) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.shinyctl_inflight",
		New: func() caddy.Module { return new(InFlightCounter) },
	}
}

func (InFlightCounter) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if isWebSocketUpgrade(r.Header) {
		return next.ServeHTTP(w, r)
	}
	inFlight.Add(1)
	defer inFlight.Add(-1)
	return next.ServeHTTP(w, r)
}

// isWebSocketUpgrade is true for a request that asks to become a WebSocket:
// "Connection" has the token upgrade and "Upgrade" has the token websocket,
// each compared without regard to case, in any of the header's lines.
func isWebSocketUpgrade(h http.Header) bool {
	return hasToken(h.Values("Connection"), "upgrade") && hasToken(h.Values("Upgrade"), "websocket")
}

func hasToken(values []string, token string) bool {
	for _, v := range values {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

var _ caddyhttp.MiddlewareHandler = InFlightCounter{}
