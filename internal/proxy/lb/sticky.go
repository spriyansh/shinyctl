package lb

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

// Sticky is the selection policy: it keeps a visitor on one copy with a
// cookie, and picks a copy for a visitor who has none (or whose copy is
// gone). Which copies exist, and their roles, come from the table.
type Sticky struct {
	// App is the table key whose copies this policy may choose from.
	App string `json:"app,omitempty"`
	// Cookie names the cookie. It is unique per app port, so two apps on one
	// host never overwrite each other's.
	Cookie string `json:"cookie,omitempty"`
	// Secret is the HMAC key of the cookie's value, which names a copy
	// without giving its address away.
	Secret string `json:"secret,omitempty"`
	// FallbackRaw chooses among the copies of one role for a new visitor;
	// least_conn when absent.
	FallbackRaw json.RawMessage `json:"fallback,omitempty" caddy:"namespace=http.reverse_proxy.selection_policies inline_key=policy"`

	fallback reverseproxy.Selector
	avail    func(*reverseproxy.Upstream) bool // replaceable in tests
}

func (*Sticky) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.selection_policies.shinyctl_sticky",
		New: func() caddy.Module { return new(Sticky) },
	}
}

func (s *Sticky) Provision(ctx caddy.Context) error {
	if s.App == "" || s.Cookie == "" || s.Secret == "" {
		return fmt.Errorf("shinyctl_sticky: app, cookie and secret are required")
	}
	if s.FallbackRaw == nil {
		s.FallbackRaw = caddyconfig.JSONModuleObject(reverseproxy.LeastConnSelection{}, "policy", "least_conn", nil)
	}
	mod, err := ctx.LoadModule(s, "FallbackRaw")
	if err != nil {
		return err
	}
	s.fallback = mod.(reverseproxy.Selector)
	if s.avail == nil {
		s.avail = func(u *reverseproxy.Upstream) bool { return u.Available() }
	}
	return nil
}

// token is the cookie value for the copy at dial.
func token(secret, dial string) string {
	h := hmac.New(sha256.New, []byte(secret))
	io.WriteString(h, dial)
	return hex.EncodeToString(h.Sum(nil))
}

// Select chooses the copy for req from pool, which is the table's list for
// the app. A copy is listed when it has a role, and up when the passive
// health checks have not marked it down.
//
//  1. The visitor's own copy, while it is listed and up: no new cookie.
//  2. Otherwise, for each role in turn (active, warm, draining), the
//     fallback policy over that role's copies that are up. The visitor gets
//     a cookie for the one chosen, so new visitors never land on a spare
//     while an active copy is up, and a draining copy gets none.
//  3. Otherwise nothing is up: the visitor's own copy if it is listed, else
//     the first listed one. A copy marked down is dialled anyway, so a lone
//     copy that restarts is served as soon as R is back, not after the
//     marks expire.
func (s *Sticky) Select(pool reverseproxy.UpstreamPool, req *http.Request, w http.ResponseWriter) *reverseproxy.Upstream {
	roles := load().roles[s.App]
	listed := func(u *reverseproxy.Upstream) bool { return roles[u.Dial] != roleNone }

	var pinned *reverseproxy.Upstream
	if c, err := req.Cookie(s.Cookie); err == nil {
		for _, u := range pool {
			if listed(u) && hmac.Equal([]byte(token(s.Secret, u.Dial)), []byte(c.Value)) {
				pinned = u
				break
			}
		}
	}
	if pinned != nil && s.avail(pinned) {
		return pinned
	}

	for _, tier := range tiers {
		var sub reverseproxy.UpstreamPool
		for _, u := range pool {
			if roles[u.Dial] == tier && s.avail(u) {
				sub = append(sub, u)
			}
		}
		if len(sub) == 0 {
			continue
		}
		if u := s.fallback.Select(sub, req, w); u != nil {
			s.setCookie(req, w, u)
			return u
		}
	}

	if pinned != nil {
		return pinned
	}
	for _, tier := range tiers {
		for _, u := range pool {
			if roles[u.Dial] == tier {
				s.setCookie(req, w, u)
				return u
			}
		}
	}
	return nil
}

// setCookie pins the visitor to u. Over HTTPS (directly, or behind a front
// proxy on this machine that says so) the cookie is Secure and SameSite=None
// so it also works inside a cross-site frame; otherwise it is Lax.
func (s *Sticky) setCookie(req *http.Request, w http.ResponseWriter, u *reverseproxy.Upstream) {
	c := &http.Cookie{Name: s.Cookie, Value: token(s.Secret, u.Dial), Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}
	https := req.TLS != nil
	if trusted, _ := caddyhttp.GetVar(req.Context(), caddyhttp.TrustedProxyVarKey).(bool); trusted {
		https = https || req.Header.Get("X-Forwarded-Proto") == "https"
	}
	if https {
		c.Secure, c.SameSite = true, http.SameSiteNoneMode
	}

	// A retry places the visitor again: only the last placement is sent.
	h := w.Header()
	kept := h["Set-Cookie"][:0]
	for _, v := range h["Set-Cookie"] {
		if !strings.HasPrefix(v, s.Cookie+"=") {
			kept = append(kept, v)
		}
	}
	h["Set-Cookie"] = kept
	h.Add("Set-Cookie", c.String())
}

var (
	_ reverseproxy.Selector = (*Sticky)(nil)
	_ caddy.Provisioner     = (*Sticky)(nil)
)
