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

// The spare stands in for an active copy that is down (rule 2).

func fallbacks() map[string]func() reverseproxy.Selector {
	return map[string]func() reverseproxy.Selector{
		"first":       func() reverseproxy.Selector { return reverseproxy.FirstSelection{} },
		"round_robin": func() reverseproxy.Selector { return &reverseproxy.RoundRobinSelection{} },
		"random":      func() reverseproxy.Selector { return reverseproxy.RandomSelection{} },
		"least_conn":  func() reverseproxy.Selector { return reverseproxy.LeastConnSelection{} },
	}
}

// Replicas 2, one active copy down: the visitor of the dead copy goes to the
// spare, not to the other active copy, whatever the fallback policy is.
func TestStandIn_DeadActiveCopysVisitorGetsTheSpare(t *testing.T) {
	p := pool("a:1", "b:2", "w:3")
	for name, mk := range fallbacks() {
		s := newSticky(t, mk(), []string{"a:1", "b:2"}, []string{"w:3"}, nil, "a:1")
		for i := 0; i < 20; i++ {
			u, c := selectFor(s, p, token(testSecret, "a:1"), false)
			if dialOf(u) != "w:3" || c == nil || c.Value != token(testSecret, "w:3") {
				t.Fatalf("%s: cookie for the dead copy -> %s, cookie %v; want the spare with its cookie", name, dialOf(u), c)
			}
		}
		// The visitor placed on the spare keeps it.
		if u, c := selectFor(s, p, token(testSecret, "w:3"), false); dialOf(u) != "w:3" || c != nil {
			t.Fatalf("%s: the visitor left the spare: %s, cookie %v", name, dialOf(u), c)
		}
		// A new visitor may get the other active copy or the spare, never the dead one.
		seen := map[string]bool{}
		for i := 0; i < 40; i++ {
			u, c := selectFor(s, p, "", false)
			if u == nil || c == nil || dialOf(u) == "a:1" {
				t.Fatalf("%s: new visitor -> %s, cookie %v", name, dialOf(u), c)
			}
			seen[u.Dial] = true
		}
		if name == "first" && (len(seen) != 1 || !seen["b:2"]) {
			t.Errorf("first: %v, want the active copy that is up first", seen)
		}
		if name == "round_robin" && len(seen) != 2 {
			t.Errorf("round_robin: %v, want both the active copy and the spare", seen)
		}
	}
}

// Replicas 1 is as before: the spare replaces the only active copy.
func TestStandIn_OneActiveCopy(t *testing.T) {
	p := pool("a:1", "w:2")
	for name, mk := range fallbacks() {
		s := newSticky(t, mk(), []string{"a:1"}, []string{"w:2"}, nil, "a:1")
		for _, cookie := range []string{"", token(testSecret, "a:1")} {
			if u, _ := selectFor(s, p, cookie, false); dialOf(u) != "w:2" {
				t.Fatalf("%s: cookie %q -> %s, want the spare", name, cookie, dialOf(u))
			}
		}
	}
}

// Two active copies down and one spare: the spare stands in for the first;
// the visitor of the second, and new visitors, get the spare too (the only
// copy that is up).
func TestStandIn_TwoDownOneSpare(t *testing.T) {
	p := pool("a:1", "b:2", "w:3")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1", "b:2"}, []string{"w:3"}, nil, "a:1", "b:2")
	for _, cookie := range []string{"", token(testSecret, "a:1"), token(testSecret, "b:2")} {
		if u, c := selectFor(s, p, cookie, false); dialOf(u) != "w:3" || c == nil {
			t.Fatalf("cookie %q -> %s, cookie %v; want the spare", cookie, dialOf(u), c)
		}
	}
}

// Two down and two spares: one spare for each, in table order; the visitors
// of the first and the second dead copy get different spares.
func TestStandIn_TwoDownTwoSpares(t *testing.T) {
	p := pool("a:1", "b:2", "w:3", "x:4")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1", "b:2"}, []string{"w:3", "x:4"}, nil, "a:1", "b:2")
	if u, _ := selectFor(s, p, token(testSecret, "a:1"), false); dialOf(u) != "w:3" {
		t.Errorf("first dead copy -> %s, want w:3", dialOf(u))
	}
	if u, _ := selectFor(s, p, token(testSecret, "b:2"), false); dialOf(u) != "x:4" {
		t.Errorf("second dead copy -> %s, want x:4", dialOf(u))
	}
}

// Only one of two spares stands in for one dead copy; the other stays out of
// the new visitors' way. A spare that is itself down is not a stand-in.
func TestStandIn_OnlyAsManyAsAreDown(t *testing.T) {
	p := pool("a:1", "b:2", "w:3", "x:4")
	s := newSticky(t, &reverseproxy.RoundRobinSelection{}, []string{"a:1", "b:2"}, []string{"w:3", "x:4"}, nil, "a:1")
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		u, _ := selectFor(s, p, "", false)
		seen[dialOf(u)] = true
	}
	if seen["x:4"] || seen["a:1"] || !seen["b:2"] || !seen["w:3"] {
		t.Errorf("new visitors went to %v, want b:2 and w:3 only", seen)
	}

	// w is down too: x is the stand-in.
	s = newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1", "b:2"}, []string{"w:3", "x:4"}, nil, "a:1", "w:3")
	if u, _ := selectFor(s, p, token(testSecret, "a:1"), false); dialOf(u) != "x:4" {
		t.Errorf("first spare down: %s, want x:4", dialOf(u))
	}

	// Every spare is down: the other active copy, as before.
	s = newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1", "b:2"}, []string{"w:3", "x:4"}, nil, "a:1", "w:3", "x:4")
	if u, _ := selectFor(s, p, token(testSecret, "a:1"), false); dialOf(u) != "b:2" {
		t.Errorf("no spare up: %s, want b:2", dialOf(u))
	}
}

// With every active copy up the spares stay out of it, as before, and a
// draining copy is only the last resort.
func TestStandIn_NothingDownChangesNothing(t *testing.T) {
	p := pool("a:1", "b:2", "w:3", "d:4")
	s := newSticky(t, &reverseproxy.RoundRobinSelection{}, []string{"a:1", "b:2"}, []string{"w:3"}, []string{"d:4"})
	for i := 0; i < 20; i++ {
		if u, _ := selectFor(s, p, "", false); dialOf(u) == "w:3" || dialOf(u) == "d:4" {
			t.Fatalf("new visitor on %s", dialOf(u))
		}
	}
	s = newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1"}, []string{"w:3"}, []string{"d:4"}, "a:1", "w:3")
	if u, _ := selectFor(s, p, "", false); dialOf(u) != "d:4" {
		t.Errorf("active and spare down -> %s, want the draining copy", dialOf(u))
	}
}

// No active copy is listed at all (an app between plans): the warm copies
// serve new visitors, as the tiers always said.
func TestStandIn_NoActiveListedUsesTheWarmTier(t *testing.T) {
	p := pool("w:1", "x:2")
	s := newSticky(t, reverseproxy.FirstSelection{}, nil, []string{"w:1", "x:2"}, nil)
	if u, c := selectFor(s, p, "", false); dialOf(u) != "w:1" || c == nil {
		t.Fatalf("no active copy listed -> %s, cookie %v; want the first warm copy", dialOf(u), c)
	}
}

// selectReq runs one Select for r and returns the choice and the cookie it set.
func selectReq(s *Sticky, p reverseproxy.UpstreamPool, r *http.Request, cookie string) (*reverseproxy.Upstream, *http.Cookie) {
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: "c", Value: cookie})
	}
	w := httptest.NewRecorder()
	u := s.Select(p, r, w)
	for _, c := range w.Result().Cookies() {
		return u, c
	}
	return u, nil
}

const browserAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"

func TestPageLoad_DrainingVisitorGetsTheNewVersion(t *testing.T) {
	p := pool("old:1", "new:2")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"new:2"}, nil, []string{"old:1"})
	r := httptest.NewRequest("GET", "/?tab=2", nil)
	r.Header.Set("Accept", browserAccept)
	if u, c := selectReq(s, p, r, token(testSecret, "old:1")); dialOf(u) != "new:2" || c == nil || c.Value != token(testSecret, "new:2") {
		t.Fatalf("a page load of a draining copy's visitor went to %s, cookie %v", dialOf(u), c)
	}
}

func TestPageLoad_TheOldPagesOwnRequestsStay(t *testing.T) {
	p := pool("old:1", "new:2")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"new:2"}, nil, []string{"old:1"})
	for _, tc := range []struct{ method, path, accept, upgrade string }{
		{"GET", "/", "*/*", ""},                                // the example's fetch before it reloads
		{"GET", "/websocket/", "", "websocket"},                // the session
		{"GET", "/", browserAccept, "websocket"},               // anything that upgrades
		{"GET", "/inner.html", browserAccept, ""},              // a frame in the page
		{"GET", "/session/abc/download/dl", browserAccept, ""}, // a download
		{"POST", "/", browserAccept, ""},
		{"GET", "/shiny-javascript-1.14.0/shiny.min.js", "*/*", ""},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		if tc.accept != "" {
			r.Header.Set("Accept", tc.accept)
		}
		if tc.upgrade != "" {
			r.Header.Set("Upgrade", tc.upgrade)
		}
		if u, c := selectReq(s, p, r, token(testSecret, "old:1")); dialOf(u) != "old:1" || c != nil {
			t.Errorf("%s %s (Accept %q, Upgrade %q) went to %s with cookie %v; want the draining copy, no cookie", tc.method, tc.path, tc.accept, tc.upgrade, dialOf(u), c)
		}
	}
}

func TestPageLoad_OnlyDrainingCopiesLetGo(t *testing.T) {
	p := pool("a:1", "b:2", "w:3")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1", "b:2"}, []string{"w:3"}, nil)
	for _, dial := range []string{"b:2", "w:3"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept", browserAccept)
		if u, c := selectReq(s, p, r, token(testSecret, dial)); dialOf(u) != dial || c != nil {
			t.Errorf("a page load pinned to %s went to %s, cookie %v", dial, dialOf(u), c)
		}
	}
}

func TestPageLoad_NothingElseUpKeepsTheDrainingCopy(t *testing.T) {
	p := pool("old:1", "new:2")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"new:2"}, nil, []string{"old:1"}, "new:2")
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept", browserAccept)
	if u, _ := selectReq(s, p, r, token(testSecret, "old:1")); dialOf(u) != "old:1" {
		t.Fatalf("with the new copy down, the page load went to %s", dialOf(u))
	}
}
