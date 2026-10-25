package lb

import (
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

// fakeClock replaces the package clock for the test.
func fakeClock(t *testing.T) *time.Time {
	t.Helper()
	cur := time.Unix(1_000_000, 0)
	now = func() time.Time { return cur }
	t.Cleanup(func() { now = time.Now })
	return &cur
}

func successorsOf(app string) map[string]string {
	out := map[string]string{}
	for dead, s := range load().succ[app] {
		out[dead] = s.dial
	}
	return out
}

func eq(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestSuccessor_DeadActiveCopyPairsWithThePromotedSpare(t *testing.T) {
	resetTable(t)
	Set(Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"w:3"}}})
	if got := successorsOf("x"); len(got) != 0 {
		t.Fatalf("pairings after the first table: %v", got)
	}
	Set(Table{"x": {Active: []string{"b:2", "w:3"}}})
	if got := successorsOf("x"); !eq(got, map[string]string{"a:1": "w:3"}) {
		t.Fatalf("pairings = %v, want a:1 -> w:3", got)
	}
}

// Two active copies die in one update: the spares take their places in order.
func TestSuccessor_TwoDeadPairInOrder(t *testing.T) {
	resetTable(t)
	Set(Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"w:3", "v:4"}}})
	Set(Table{"x": {Active: []string{"w:3", "v:4"}}})
	if got := successorsOf("x"); !eq(got, map[string]string{"a:1": "w:3", "b:2": "v:4"}) {
		t.Fatalf("pairings = %v, want a:1 -> w:3 and b:2 -> v:4", got)
	}
	// One spare for two: the first dead copy gets it, the other has none.
	Set(nil)
	Set(Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"w:3"}}})
	Set(Table{"x": {Active: []string{"w:3"}}})
	if got := successorsOf("x"); !eq(got, map[string]string{"a:1": "w:3"}) {
		t.Fatalf("pairings = %v, want a:1 -> w:3 only", got)
	}
}

// Only a copy that left the table is paired: one that became warm or
// draining is still listed and keeps its visitors. A scale-down with no
// spare promoted pairs nothing.
func TestSuccessor_OnlyCopiesThatLeftTheTable(t *testing.T) {
	resetTable(t)
	Set(Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"w:3"}}})
	Set(Table{"x": {Active: []string{"b:2", "w:3"}, Warm: []string{"a:1"}}}) // demoted
	if got := successorsOf("x"); len(got) != 0 {
		t.Errorf("a demoted copy was paired: %v", got)
	}
	Set(nil)
	Set(Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"w:3"}}})
	Set(Table{"x": {Active: []string{"a:1"}, Warm: []string{"w:3"}}}) // scale-down of b
	if got := successorsOf("x"); len(got) != 0 {
		t.Errorf("a scale-down without a promotion was paired: %v", got)
	}
}

// A scale-down that removes an active copy while a spare becomes active for
// another reason pairs them too: the removed copy's visitors go to a copy
// that serves, which is as good a place as any.
func TestSuccessor_ScaleDownWithAPromotionPairsToo(t *testing.T) {
	resetTable(t)
	Set(Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"w:3"}}})
	Set(Table{"x": {Active: []string{"a:1", "w:3"}}})
	if got := successorsOf("x"); !eq(got, map[string]string{"b:2": "w:3"}) {
		t.Fatalf("pairings = %v, want b:2 -> w:3", got)
	}
}

func TestSuccessor_KeptUntilListedAgainExpiredOrAppGone(t *testing.T) {
	clock := fakeClock(t)
	resetTable(t)
	Set(Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"w:3"}}})
	Set(Table{"x": {Active: []string{"b:2", "w:3"}}})

	*clock = clock.Add(30 * time.Second)
	Set(Table{"x": {Active: []string{"b:2", "w:3"}, Warm: []string{"z:9"}}}) // another update
	if got := successorsOf("x"); !eq(got, map[string]string{"a:1": "w:3"}) {
		t.Fatalf("after another update: %v, want the pairing kept", got)
	}

	// The dead copy restarted on the same port and is listed again as warm.
	Set(Table{"x": {Active: []string{"b:2", "w:3"}, Warm: []string{"a:1"}}})
	if got := successorsOf("x"); len(got) != 0 {
		t.Errorf("after it was listed again: %v, want none", got)
	}

	// Sixty seconds.
	Set(Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"w:3"}}})
	Set(Table{"x": {Active: []string{"b:2", "w:3"}}})
	*clock = clock.Add(59 * time.Second)
	if len(successorsOf("x")) != 1 {
		t.Fatal("pairing gone after 59s")
	}
	*clock = clock.Add(2 * time.Second)
	Set(Table{"x": {Active: []string{"b:2", "w:3"}}})
	if got := successorsOf("x"); len(got) != 0 {
		t.Errorf("after 61s: %v, want none", got)
	}

	// The app is gone from the table.
	Set(Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"w:3"}}})
	Set(Table{"x": {Active: []string{"b:2", "w:3"}}})
	Set(Table{"y": {Active: []string{"c:5"}}})
	if got := successorsOf("x"); len(got) != 0 {
		t.Errorf("after the app left: %v, want none", got)
	}
}

func TestSuccessor_AppsAreIsolated(t *testing.T) {
	resetTable(t)
	Set(Table{"x": {Active: []string{"a:1"}, Warm: []string{"w:2"}}, "y": {Active: []string{"a:1"}, Warm: []string{"w:2"}}})
	Set(Table{"x": {Active: []string{"w:2"}}, "y": {Active: []string{"a:1"}, Warm: []string{"w:2"}}})
	if len(successorsOf("x")) != 1 || len(successorsOf("y")) != 0 {
		t.Errorf("x: %v, y: %v", successorsOf("x"), successorsOf("y"))
	}
}

// Select: a cookie for a copy that left the table goes to its partner.
func TestSuccessor_SelectSendsTheDeadCopysVisitorToItsPartner(t *testing.T) {
	p := pool("b:2", "w:3")
	for name, mk := range fallbacks() {
		s := newSticky(t, mk(), []string{"a:1", "b:2"}, []string{"w:3"}, nil)
		Set(Table{"x": {Active: []string{"b:2", "w:3"}}})
		for i := 0; i < 20; i++ {
			u, c := selectFor(s, p, token(testSecret, "a:1"), false)
			if dialOf(u) != "w:3" || c == nil || c.Value != token(testSecret, "w:3") {
				t.Fatalf("%s: dead copy's cookie -> %s, cookie %v; want the partner with its cookie", name, dialOf(u), c)
			}
		}
		// A cookie that names nothing known is placed as before.
		seen := map[string]bool{}
		for i := 0; i < 20; i++ {
			u, _ := selectFor(s, p, token("other", "a:1"), false)
			seen[dialOf(u)] = true
		}
		if name == "first" && (len(seen) != 1 || !seen["b:2"]) {
			t.Errorf("first: a foreign cookie -> %v, want b:2", seen)
		}
	}
}

func TestSuccessor_SelectFallsBackWhenThePartnerIsDownOrUnlisted(t *testing.T) {
	p := pool("b:2", "w:3")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1", "b:2"}, []string{"w:3"}, nil)
	Set(Table{"x": {Active: []string{"b:2", "w:3"}}})
	s.avail = func(u *reverseproxy.Upstream) bool { return u.Dial != "w:3" }
	if u, _ := selectFor(s, p, token(testSecret, "a:1"), false); dialOf(u) != "b:2" {
		t.Errorf("partner down -> %s, want the usual rules (b:2)", dialOf(u))
	}
	// The partner left the table too.
	Set(Table{"x": {Active: []string{"b:2"}}})
	s.avail = func(*reverseproxy.Upstream) bool { return true }
	if u, _ := selectFor(s, pool("b:2", "w:3"), token(testSecret, "a:1"), false); dialOf(u) != "b:2" {
		t.Errorf("partner unlisted -> %s, want b:2", dialOf(u))
	}
}

func TestSuccessor_SelectExpires(t *testing.T) {
	clock := fakeClock(t)
	p := pool("b:2", "w:3")
	s := newSticky(t, reverseproxy.FirstSelection{}, []string{"a:1", "b:2"}, []string{"w:3"}, nil)
	Set(Table{"x": {Active: []string{"b:2", "w:3"}}})
	*clock = clock.Add(61 * time.Second) // no further update: the snapshot still holds it
	if u, _ := selectFor(s, p, token(testSecret, "a:1"), false); dialOf(u) != "b:2" {
		t.Errorf("after 61s -> %s, want the usual rules (b:2)", dialOf(u))
	}
}
