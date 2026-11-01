package lb

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// activeConns is the set of connections between net/http reading a request
// and writing its response, as the server reports them: it adds a connection
// when it turns active and drops it in every other state (idle, closed,
// hijacked). Counting connections rather than handler calls matters: net/http
// writes the response after the handlers return, and a stop that saw the
// count at zero in that gap would exit before the client got its answer.
//
// A request that has arrived but is not read yet when the listener closes is
// not counted. Counting a connection from StateNew would hold every stop for
// the spare connections browsers open.
type activeConns struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// track is the server's ConnState hook. It only touches the map, so it never
// holds up the connection.
func (a *activeConns) track(c net.Conn, s http.ConnState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s != http.StateActive {
		delete(a.conns, c)
		return
	}
	if a.conns == nil {
		a.conns = map[net.Conn]struct{}{}
	}
	a.conns[c] = struct{}{}
}

func (a *activeConns) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.conns)
}

// active is shared by the servers of every configuration the engine loads: a
// reload builds new servers while the old ones may still be answering.
var active activeConns

// InFlight is the number of requests being answered now: read, and not yet
// written. A load balancer that is stopping waits for it to reach zero: the
// engine closes its listeners at once on a stop, but answers the requests in
// flight in goroutines that nothing waits for before the process exits. A
// connection that has become a WebSocket is not counted: a session never
// finishes, and is meant to end when the load balancer stops.
func InFlight() int { return active.count() }

// InFlightCounter is the handler that makes the count work: the first one of
// every route. It does nothing to a request; its Provision registers the
// connection hook on the server it belongs to.
type InFlightCounter struct{}

func (InFlightCounter) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.shinyctl_inflight",
		New: func() caddy.Module { return new(InFlightCounter) },
	}
}

// Provision registers the hook here, because the handler is provisioned with
// its server in the context, whichever way the configuration got loaded (a
// reload and a validation included). Connection states are only reported for
// HTTP/1: the engine serves HTTP/2 connections without them, so any other
// protocol setting is refused rather than counted wrongly.
func (InFlightCounter) Provision(ctx caddy.Context) error {
	srv, ok := ctx.Value(caddyhttp.ServerCtxKey).(*caddyhttp.Server)
	if !ok {
		return errors.New("shinyctl_inflight must be a route handler of a server")
	}
	if !slices.Equal(srv.Protocols, []string{"h1"}) || srv.ListenProtocols != nil {
		return fmt.Errorf("shinyctl_inflight counts HTTP/1 only; the server's protocols are %v", srv.Protocols)
	}
	srv.RegisterConnState(active.track)
	return nil
}

func (InFlightCounter) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	return next.ServeHTTP(w, r)
}

var (
	_ caddy.Provisioner           = InFlightCounter{}
	_ caddyhttp.MiddlewareHandler = InFlightCounter{}
)
