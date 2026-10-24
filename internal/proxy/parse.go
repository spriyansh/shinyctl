package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
)

// Parse reads back the apps a config serves: the inverse of Render for what
// shinyctl-api needs to know about a load balancer it didn't start, or has
// restarted. Name and Group are not in the config and stay empty. A config
// of any other shape is an error, so the caller reloads instead of
// trusting it.
func Parse(cfg []byte) ([]App, error) {
	var c struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Listen []string `json:"listen"`
					Routes []struct {
						Handle []json.RawMessage `json:"handle"`
					} `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return nil, fmt.Errorf("parsing the load balancer's settings: %w", err)
	}

	var apps []App
	for key, s := range c.Apps.HTTP.Servers {
		if len(s.Listen) != 1 {
			return nil, fmt.Errorf("server %q listens on %d addresses, want 1", key, len(s.Listen))
		}
		_, addr, ok := strings.Cut(s.Listen[0], "/")
		if !ok {
			return nil, fmt.Errorf("server %q: unexpected listen address %q", key, s.Listen[0])
		}
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", key, err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", key, err)
		}

		app := App{Key: key, Host: host, Port: port}
		found := false
		for _, r := range s.Routes {
			for _, h := range r.Handle {
				var rp struct {
					Handler   string `json:"handler"`
					Upstreams []struct {
						Dial string `json:"dial"`
					} `json:"upstreams"`
				}
				if err := json.Unmarshal(h, &rp); err != nil || rp.Handler != "reverse_proxy" {
					continue
				}
				found = true
				for _, u := range rp.Upstreams {
					app.Upstreams = append(app.Upstreams, u.Dial)
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("server %q has no reverse_proxy handler", key)
		}
		apps = append(apps, app)
	}

	slices.SortFunc(apps, func(a, b App) int { return strings.Compare(a.Key, b.Key) })
	return apps, nil
}
