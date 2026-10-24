package lb

import (
	"net/http"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

// Copies is the upstream source: the copies listed for an app, read from the
// table for every request.
type Copies struct {
	App string `json:"app,omitempty"`
}

func (Copies) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.shinyctl_copies",
		New: func() caddy.Module { return new(Copies) },
	}
}

// GetUpstreams returns a fresh Upstream for each listed copy, in the table's
// order. The handler fills in each one's shared state before using it, so
// values must not be reused between requests. It never fails: an app with no
// copies gets an empty list, which the handler waits out and then answers
// with the restarting page.
func (c *Copies) GetUpstreams(*http.Request) ([]*reverseproxy.Upstream, error) {
	order := load().order[c.App]
	out := make([]*reverseproxy.Upstream, len(order))
	for i, dial := range order {
		out[i] = &reverseproxy.Upstream{Dial: dial}
	}
	return out, nil
}

var _ reverseproxy.UpstreamSource = (*Copies)(nil)
