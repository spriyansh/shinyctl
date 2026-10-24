package lb

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

const testSecret = "k"

func pool(dials ...string) reverseproxy.UpstreamPool {
	var p reverseproxy.UpstreamPool
	for _, d := range dials {
		p = append(p, &reverseproxy.Upstream{Host: new(reverseproxy.Host), Dial: d})
	}
	return p
}

// newSticky returns a policy for app "x" with the table set to the given
// roles and the dials in down marked unavailable. It resets the table when
// the test ends.
func newSticky(t *testing.T, fb reverseproxy.Selector, active, warm, draining []string, down ...string) *Sticky {
	t.Helper()
	resetTable(t)
	Set(Table{"x": {Active: active, Warm: warm, Draining: draining}})
	bad := map[string]bool{}
	for _, d := range down {
		bad[d] = true
	}
	return &Sticky{
		App: "x", Cookie: "c", Secret: testSecret, fallback: fb,
		avail: func(u *reverseproxy.Upstream) bool { return !bad[u.Dial] },
	}
}

func dialOf(u *reverseproxy.Upstream) string {
	if u == nil {
		return "<nil>"
	}
	return u.Dial
}

// selectFor runs one Select and returns the choice and the cookie it set.
func selectFor(s *Sticky, p reverseproxy.UpstreamPool, cookie string, tlsReq bool) (*reverseproxy.Upstream, *http.Cookie) {
	r := httptest.NewRequest("GET", "/", nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: "c", Value: cookie})
	}
	if tlsReq {
		r.TLS = &tls.ConnectionState{}
	}
	w := httptest.NewRecorder()
	u := s.Select(p, r, w)
	for _, c := range w.Result().Cookies() {
		return u, c
	}
	return u, nil
}

func TestNewVisitorsNeverWarmWhileActiveUp(t *testing.T) {
	p := pool("a:1", "b:2", "c:3")
	s := newSticky(t, &reverseproxy.RoundRobinSelection{}, []string{"a:1", "b:2"}, []string{"c:3"}, nil)
	for i := 0; i < 50; i++ {
		u, c := selectFor(s, p, "", false)
		if dialOf(u) == "c:3" || c == nil {
			t.Fatalf("got %s, cookie %v", dialOf(u), c)
		}
	}
}

func TestCookieSticksInEveryTier(t *testing.T) {
	p := pool("a:1", "b:2", "c:3")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, []string{"b:2"}, []string{"c:3"})
	for _, d := range []string{"a:1", "b:2", "c:3"} {
		u, c := selectFor(s, p, token(testSecret, d), false)
		if dialOf(u) != d || c != nil {
			t.Fatalf("cookie for %s -> %s, new cookie %v", d, dialOf(u), c)
		}
	}
}

func TestCookieForUnlistedOrBadIsIgnored(t *testing.T) {
	p := pool("a:1", "b:2")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, nil, nil)

	// b is in the pool but has no role: its cookie must not pin to it.
	if u, c := selectFor(s, p, token(testSecret, "b:2"), false); dialOf(u) != "a:1" || c == nil {
		t.Fatalf("unlisted copy: %s, cookie %v", dialOf(u), c)
	}
	if u, c := selectFor(s, p, token("other", "a:1"), false); dialOf(u) != "a:1" || c == nil {
		t.Fatalf("wrong secret should get a fresh cookie: %s, cookie %v", dialOf(u), c)
	}
	if u, c := selectFor(s, p, "garbage", false); dialOf(u) != "a:1" || c == nil {
		t.Fatalf("garbage should get a fresh cookie: %s, cookie %v", dialOf(u), c)
	}
}

func TestDrainingKeepsPinnedButGetsNoNewVisitors(t *testing.T) {
	p := pool("a:1", "b:2")
	s := newSticky(t, &reverseproxy.RoundRobinSelection{}, []string{"b:2"}, nil, []string{"a:1"})
	if u, _ := selectFor(s, p, token(testSecret, "a:1"), false); dialOf(u) != "a:1" {
		t.Fatal("a pinned visitor left the draining copy")
	}
	for i := 0; i < 20; i++ {
		if u, _ := selectFor(s, p, "", false); dialOf(u) != "b:2" {
			t.Fatal("a new visitor went to the draining copy")
		}
	}
}

func TestTierOrderActiveWarmDraining(t *testing.T) {
	p := pool("a:1", "w:2", "d:3")

	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, []string{"w:2"}, []string{"d:3"}, "a:1")
	if u, c := selectFor(s, p, token(testSecret, "a:1"), false); dialOf(u) != "w:2" || c == nil || c.Value != token(testSecret, "w:2") {
		t.Fatalf("active down -> warm, got %s, cookie %v", dialOf(u), c)
	}

	s = newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, []string{"w:2"}, []string{"d:3"}, "a:1", "w:2")
	if u, _ := selectFor(s, p, "", false); dialOf(u) != "d:3" {
		t.Fatalf("active and warm down -> draining, got %s", dialOf(u))
	}
}

func TestCookieAttributes(t *testing.T) {
	p := pool("a:1")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, nil, nil)

	_, c := selectFor(s, p, "", false)
	if c == nil || c.Name != "c" || c.Path != "/" || !c.HttpOnly || c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("plain: %+v", c)
	}
	_, c = selectFor(s, p, "", true)
	if c == nil || c.Path != "/" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteNoneMode {
		t.Fatalf("tls: %+v", c)
	}
}

// forwarded runs Select for a request that says it came over HTTPS through a
// front proxy, with the proxy trusted or not.
func forwarded(t *testing.T, s *Sticky, p reverseproxy.UpstreamPool, trusted bool, proto string) *http.Cookie {
	t.Helper()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-Proto", proto)
	ctx := context.WithValue(r.Context(), caddy.ReplacerCtxKey, caddy.NewReplacer())
	ctx = context.WithValue(ctx, caddyhttp.VarsCtxKey, map[string]any{caddyhttp.TrustedProxyVarKey: trusted})
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	s.Select(p, r, w)
	for _, c := range w.Result().Cookies() {
		return c
	}
	return nil
}

func TestCookieAttributes_ForwardedProto(t *testing.T) {
	p := pool("a:1")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, nil, nil)

	if c := forwarded(t, s, p, true, "https"); c == nil || !c.Secure || c.SameSite != http.SameSiteNoneMode {
		t.Errorf("trusted proxy saying https: %+v", c)
	}
	if c := forwarded(t, s, p, true, "http"); c == nil || c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("trusted proxy saying http: %+v", c)
	}
	if c := forwarded(t, s, p, false, "https"); c == nil || c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("untrusted proxy saying https must be ignored: %+v", c)
	}
}

func TestFallbackPolicies(t *testing.T) {
	p := pool("a:1", "b:2", "c:3")
	for name, fb := range map[string]reverseproxy.Selector{
		"least_conn":  reverseproxy.LeastConnSelection{},
		"round_robin": &reverseproxy.RoundRobinSelection{},
		"random":      reverseproxy.RandomSelection{},
		"first":       reverseproxy.FirstSelection{},
	} {
		s := newSticky(t, fb, []string{"a:1", "b:2"}, []string{"c:3"}, nil)
		seen := map[string]int{}
		for i := 0; i < 60; i++ {
			u, c := selectFor(s, p, "", false)
			if u == nil || c == nil || u.Dial == "c:3" {
				t.Fatalf("%s: bad pick %s", name, dialOf(u))
			}
			seen[u.Dial]++
		}
		if name == "first" && seen["a:1"] != 60 {
			t.Errorf("first: %v, want 60 on the first active copy", seen)
		}
		if name == "round_robin" && (seen["a:1"] != 30 || seen["b:2"] != 30) {
			t.Errorf("round_robin: %v, want 30 each", seen)
		}
	}
}

func TestSelect_ConcurrentUse(t *testing.T) {
	p := pool("a:1", "b:2", "c:3")
	s := newSticky(t, &reverseproxy.RoundRobinSelection{}, []string{"a:1", "b:2"}, []string{"c:3"}, nil)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				cookie := ""
				if i%2 == 0 {
					cookie = token(testSecret, "b:2")
				}
				if g == 0 && i%50 == 0 {
					Set(Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"c:3"}}})
				}
				if u, _ := selectFor(s, p, cookie, false); u == nil {
					t.Error("no copy chosen")
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestSelect_EmptyPool(t *testing.T) {
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, nil, nil)
	for _, p := range []reverseproxy.UpstreamPool{nil, {}} {
		if u, c := selectFor(s, p, "", false); u != nil || c != nil {
			t.Errorf("empty pool: %s, cookie %v; want nothing", dialOf(u), c)
		}
	}
}

func TestProvision(t *testing.T) {
	ctx, cancel := caddy.NewContext(caddy.Context{Context: context.Background()})
	defer cancel()

	for name, s := range map[string]*Sticky{
		"no app":    {Cookie: "c", Secret: "k"},
		"no cookie": {App: "x", Secret: "k"},
		"no secret": {App: "x", Cookie: "c"},
	} {
		err := s.Provision(ctx)
		if err == nil || !strings.Contains(err.Error(), "app, cookie and secret are required") {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	s := &Sticky{App: "x", Cookie: "c", Secret: "k", FallbackRaw: []byte(`{"policy":"nonsense"}`)}
	if err := s.Provision(ctx); err == nil {
		t.Error("an unknown fallback should be refused")
	}

	s = &Sticky{App: "x", Cookie: "c", Secret: "k"}
	if err := s.Provision(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%T", s.fallback); !strings.HasSuffix(got, "LeastConnSelection") {
		t.Errorf("default fallback = %s, want least_conn", got)
	}
}

// The tests below cover the last-resort rule and the table's isolation.

func TestRule3_LoneCopyMarkedDown(t *testing.T) {
	p := pool("a:1")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, nil, nil, "a:1")
	u, c := selectFor(s, p, "", false)
	if dialOf(u) != "a:1" || c == nil || c.Value != token(testSecret, "a:1") {
		t.Fatalf("lone copy marked down: %s, cookie %v; want it dialled, with a cookie", dialOf(u), c)
	}
}

func TestRule3_PinnedMarkedDown(t *testing.T) {
	p := pool("a:1", "b:2")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1", "b:2"}, nil, nil, "a:1", "b:2")
	u, c := selectFor(s, p, token(testSecret, "b:2"), false)
	if dialOf(u) != "b:2" || c != nil {
		t.Fatalf("pinned copy down with the other down too: %s, cookie %v; want the pinned copy, no new cookie", dialOf(u), c)
	}
}

func TestRule2_BeatsRule3(t *testing.T) {
	p := pool("a:1", "w:2")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, []string{"w:2"}, nil, "a:1")
	u, c := selectFor(s, p, token(testSecret, "a:1"), false)
	if dialOf(u) != "w:2" || c == nil || c.Value != token(testSecret, "w:2") {
		t.Fatalf("pinned down, warm up: %s, cookie %v; want the warm copy with its cookie", dialOf(u), c)
	}
}

func TestRule3_FirstListedWhenNothingIsPinned(t *testing.T) {
	p := pool("w:2", "a:1")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, []string{"w:2"}, nil, "a:1", "w:2")
	if u, _ := selectFor(s, p, "", false); dialOf(u) != "a:1" {
		t.Fatalf("everything down: %s, want the active copy first", dialOf(u))
	}
}

func TestRetryLeavesOneCookie(t *testing.T) {
	p := pool("a:1", "b:2")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1", "b:2"}, nil, nil)
	bad := map[string]bool{}
	s.avail = func(u *reverseproxy.Upstream) bool { return !bad[u.Dial] }

	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	if u := s.Select(p, r, w); dialOf(u) != "a:1" {
		t.Fatalf("first pick: %s", dialOf(u))
	}
	bad["a:1"] = true // the dial failed; the handler tries again
	if u := s.Select(p, r, w); dialOf(u) != "b:2" {
		t.Fatalf("second pick: %s", dialOf(u))
	}

	got := w.Header().Values("Set-Cookie")
	if len(got) != 1 || !strings.Contains(got[0], token(testSecret, "b:2")) {
		t.Fatalf("Set-Cookie = %v, want exactly one, for the second pick", got)
	}
}

func TestRetryKeepsOtherCookies(t *testing.T) {
	p := pool("a:1")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, nil, nil)
	w := httptest.NewRecorder()
	w.Header().Add("Set-Cookie", "other=1")
	s.Select(p, httptest.NewRequest("GET", "/", nil), w)
	if got := w.Header().Values("Set-Cookie"); len(got) != 2 || got[0] != "other=1" {
		t.Fatalf("Set-Cookie = %v, want the app's own cookie kept", got)
	}
}

func TestAppsAreIsolated(t *testing.T) {
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, nil, nil)
	Set(Table{"x": {Active: []string{"a:1"}}, "y": {Active: []string{"b:2"}, Warm: []string{"c:3"}}})

	// The pool holds a copy of app y; a policy for x must never choose it.
	p := pool("b:2", "c:3", "a:1")
	for i := 0; i < 10; i++ {
		if u, _ := selectFor(s, p, "", false); dialOf(u) != "a:1" {
			t.Fatalf("policy for x chose %s", dialOf(u))
		}
	}
	if u, _ := selectFor(s, p, token(testSecret, "b:2"), false); dialOf(u) != "a:1" {
		t.Fatalf("a cookie for y's copy pinned x's visitor to %s", dialOf(u))
	}
	if u, _ := selectFor(s, pool("b:2"), "", false); u != nil {
		t.Fatalf("pool with only y's copy: chose %s", dialOf(u))
	}
}

func TestTableChangeTakesEffectNextSelect(t *testing.T) {
	p := pool("a:1", "b:2")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, []string{"b:2"}, nil)

	if u, _ := selectFor(s, p, "", false); dialOf(u) != "a:1" {
		t.Fatalf("before: %s", dialOf(u))
	}
	Set(Table{"x": {Active: []string{"b:2"}}})
	if u, _ := selectFor(s, p, "", false); dialOf(u) != "b:2" {
		t.Fatalf("after Set(active b): new visitor on %s", dialOf(u))
	}
	// A visitor pinned to a stays on a only while a is still listed.
	Set(Table{"x": {Active: []string{"b:2"}, Draining: []string{"a:1"}}})
	if u, c := selectFor(s, p, token(testSecret, "a:1"), false); dialOf(u) != "a:1" || c != nil {
		t.Fatalf("a draining: pinned visitor on %s, cookie %v", dialOf(u), c)
	}
	Set(Table{"x": {Active: []string{"b:2"}}})
	if u, c := selectFor(s, p, token(testSecret, "a:1"), false); dialOf(u) != "b:2" || c == nil {
		t.Fatalf("a removed: pinned visitor on %s, cookie %v", dialOf(u), c)
	}
}
