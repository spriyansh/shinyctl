package proxy

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var testSettings = func() Settings {
	s := SettingsFor("/home/me/.shinyctl/proxy")
	s.Secret = testSecret
	return s
}()

var testApps = []App{
	{Key: "/home/me/apps/explorer/state.json", Name: "explorer", Group: "lab", Host: "0.0.0.0", Port: 8765},
	{Key: "/home/me/apps/maps/state.json", Name: "maps", Group: "lab", Host: "127.0.0.1", Port: 8080, LBPolicy: "round-robin"},
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRender_Golden(t *testing.T) {
	cases := []struct {
		golden string
		apps   []App
	}{
		{"two-apps.json", testApps},
		{"no-apps.json", nil},
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			got, err := Render(testSettings, c.apps)
			if err != nil {
				t.Fatal(err)
			}
			if want := readGolden(t, c.golden); string(got) != want {
				t.Errorf("Render output differs from testdata/%s:\n%s", c.golden, got)
			}
		})
	}
}

func TestRender_IPv6(t *testing.T) {
	for host, want := range map[string]string{"::1": "tcp6/[::1]:8080", "::": "tcp6/[::]:8080"} {
		cfg, err := Render(testSettings, []App{{Key: "k", Host: host, Port: 8080}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(cfg), `"`+want+`"`) {
			t.Errorf("host %s: config does not listen on %s:\n%s", host, want, cfg)
		}
	}
}

func TestRender_Deterministic(t *testing.T) {
	a, err := Render(testSettings, testApps)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(testSettings, []App{testApps[1], testApps[0]})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Error("the same apps in a different order render differently")
	}
}

// decodeServers returns the rendered servers as generic JSON.
func decodeServers(t *testing.T, cfg []byte) map[string]map[string]any {
	t.Helper()
	var c struct {
		Apps struct {
			HTTP struct {
				Servers map[string]map[string]any `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	return c.Apps.HTTP.Servers
}

func TestRender_EveryServerHasRetryAndStream(t *testing.T) {
	cfg, err := Render(testSettings, testApps)
	if err != nil {
		t.Fatal(err)
	}
	servers := decodeServers(t, cfg)
	if len(servers) != 2 {
		t.Fatalf("servers = %d, want 2", len(servers))
	}
	for key, s := range servers {
		routes := s["routes"].([]any)
		handlers := routes[0].(map[string]any)["handle"].([]any)
		var rp map[string]any
		for _, h := range handlers {
			if m := h.(map[string]any); m["handler"] == "reverse_proxy" {
				rp = m
			}
		}
		if rp == nil {
			t.Fatalf("%s: no reverse_proxy handler", key)
		}
		if got := rp["stream_close_delay"]; got != "12h" {
			t.Errorf("%s: stream_close_delay = %v, want 12h", key, got)
		}
		lb := rp["load_balancing"].(map[string]any)
		if lb["try_duration"] != "15s" || lb["try_interval"] != "250ms" {
			t.Errorf("%s: load_balancing = %v, want 15s / 250ms", key, lb)
		}
	}
}

func TestRender_TryDurationSetting(t *testing.T) {
	s := testSettings
	s.TryDuration = time.Second
	cfg, err := Render(s, testApps[:1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), `"try_duration": "1s"`) {
		t.Errorf("config does not carry try_duration 1s:\n%s", cfg)
	}
}

func TestRender_ErrorPageCovers502And503(t *testing.T) {
	cfg, err := Render(testSettings, testApps[:1])
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range decodeServers(t, cfg) {
		routes := s["errors"].(map[string]any)["routes"].([]any)
		if len(routes) != 2 {
			t.Fatalf("error routes = %d, want 2", len(routes))
		}
		match := routes[1].(map[string]any)["match"].([]any)[0].(map[string]any)["vars"].(map[string]any)
		codes := match["{http.error.status_code}"].([]any)
		if len(codes) != 2 || codes[0] != "502" || codes[1] != "503" {
			t.Errorf("page matches %v, want 502 and 503", codes)
		}
	}
}

func TestRestartingPage(t *testing.T) {
	if strings.Contains(restartingPage, "{") {
		t.Error("the page contains a brace, which the response body would expand as a placeholder")
	}
	if regexp.MustCompile(`(?i)caddy`).MatchString(restartingPage) {
		t.Error("the page names the software")
	}
	if !strings.Contains(restartingPage, "This app is restarting") {
		t.Error("the page does not say the app is restarting")
	}
}

func TestRender_Validates(t *testing.T) {
	for n := 0; n <= 2; n++ {
		cfg, err := Render(testSettings, testApps[:n])
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(cfg); err != nil {
			t.Errorf("%d app(s): Validate: %v", n, err)
		}
	}
	if err := Validate([]byte("not json")); err == nil {
		t.Error("Validate accepted garbage")
	}
}

func TestRender_TrustsLoopbackForwarders(t *testing.T) {
	cfg, err := Render(testSettings, testApps)
	if err != nil {
		t.Fatal(err)
	}
	for key, s := range decodeServers(t, cfg) {
		tp, ok := s["trusted_proxies"].(map[string]any)
		if !ok || tp["source"] != "static" {
			t.Fatalf("%s: trusted_proxies = %v, want a static source", key, s["trusted_proxies"])
		}
		ranges := tp["ranges"].([]any)
		if len(ranges) != 2 || ranges[0] != "127.0.0.0/8" || ranges[1] != "::1/128" {
			t.Errorf("%s: trusted ranges = %v, want only loopback", key, ranges)
		}
	}
}

// TestRender_CanonicalListenAddress: a host written in a longer form listens
// on, and compares as, the address the OS names in its errors.
func TestRender_CanonicalListenAddress(t *testing.T) {
	cases := map[string]string{
		"::ffff:127.0.0.1": "tcp4/127.0.0.1:8080",
		"0:0:0:0:0:0:0:1":  "tcp6/[::1]:8080",
		"::0":              "tcp6/[::]:8080",
		"0000::1":          "tcp6/[::1]:8080",
		"127.0.0.1":        "tcp4/127.0.0.1:8080",
	}
	for host, want := range cases {
		cfg, err := Render(testSettings, []App{{Key: "k", Host: host, Port: 8080}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(cfg), `"`+want+`"`) {
			t.Errorf("host %s: config does not listen on %s:\n%s", host, want, cfg)
		}
		if err := Validate(cfg); err != nil {
			t.Errorf("host %s: Validate: %v", host, err)
		}
	}
}

// reverseProxyOf returns the reverse_proxy handler of the server at key.
func reverseProxyOf(t *testing.T, cfg []byte, key string) map[string]any {
	t.Helper()
	s, ok := decodeServers(t, cfg)[key]
	if !ok {
		t.Fatalf("no server %q", key)
	}
	for _, h := range s["routes"].([]any)[0].(map[string]any)["handle"].([]any) {
		if m := h.(map[string]any); m["handler"] == "reverse_proxy" {
			return m
		}
	}
	t.Fatalf("%s: no reverse_proxy handler", key)
	return nil
}

func TestRender_PassiveChecksAndDynamicCopies(t *testing.T) {
	cfg, err := Render(testSettings, testApps)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range testApps {
		rp := reverseProxyOf(t, cfg, a.Key)
		if _, ok := rp["upstreams"]; ok {
			t.Errorf("%s: lists upstreams itself; the copies come from the table", a.Key)
		}
		dyn := rp["dynamic_upstreams"].(map[string]any)
		if dyn["source"] != "shinyctl_copies" || dyn["app"] != a.Key {
			t.Errorf("%s: dynamic_upstreams = %v", a.Key, dyn)
		}
		passive := rp["health_checks"].(map[string]any)["passive"].(map[string]any)
		if passive["fail_duration"] != "2s" || passive["max_fails"] != float64(1) {
			t.Errorf("%s: passive checks = %v, want 2s and 1 failure", a.Key, passive)
		}
		policy := rp["load_balancing"].(map[string]any)["selection_policy"].(map[string]any)
		if policy["policy"] != "shinyctl_sticky" || policy["app"] != a.Key || policy["secret"] != testSecret {
			t.Errorf("%s: selection_policy = %v", a.Key, policy)
		}
	}
}

func TestRender_FallbackPolicies(t *testing.T) {
	for lbPolicy, want := range map[string]string{
		"": "least_conn", "least-conn": "least_conn", "round-robin": "round_robin", "random": "random", "first": "first",
	} {
		app := plainApp("k", 8080)
		app.LBPolicy = lbPolicy
		cfg, err := Render(testSettings, []App{app})
		if err != nil {
			t.Fatalf("%q: %v", lbPolicy, err)
		}
		policy := reverseProxyOf(t, cfg, "k")["load_balancing"].(map[string]any)["selection_policy"].(map[string]any)
		if got := policy["fallback"].(map[string]any)["policy"]; got != want {
			t.Errorf("lb-policy %q: fallback = %v, want %s", lbPolicy, got, want)
		}
		if err := Validate(cfg); err != nil {
			t.Errorf("lb-policy %q: Validate: %v", lbPolicy, err)
		}
	}
}

func TestRender_UnknownPolicyIsAnError(t *testing.T) {
	for _, bad := range []string{"least_conn", "nonsense", "ip_hash"} {
		app := plainApp("k", 8080)
		app.LBPolicy = bad
		if _, err := Render(testSettings, []App{app}); err == nil {
			t.Errorf("lb-policy %q rendered", bad)
		}
	}
}

func TestRender_NeedsSecret(t *testing.T) {
	s := testSettings
	s.Secret = ""
	if _, err := Render(s, testApps); err == nil || !strings.Contains(err.Error(), "no secret") {
		t.Errorf("Render without a secret: %v, want a refusal", err)
	}
	if _, err := Render(s, nil); err != nil {
		t.Errorf("the bootstrap needs no secret: %v", err)
	}
}

func TestCookieName(t *testing.T) {
	if got := CookieName(8765); got != "lb_8765" {
		t.Errorf("CookieName(8765) = %q", got)
	}
}

// Two apps on one host never share a cookie name; two on the same port but
// different hosts do, which is fine because a browser keeps cookies per host.
func TestRender_CookieUniquePerPort(t *testing.T) {
	apps := []App{
		{Key: "a", Name: "a", Host: "127.0.0.1", Port: 8080},
		{Key: "b", Name: "b", Host: "127.0.0.1", Port: 8081},
		{Key: "c", Name: "c", Host: "10.0.0.1", Port: 8080},
	}
	cfg, err := Render(testSettings, apps)
	if err != nil {
		t.Fatal(err)
	}
	cookieOf := func(key string) string {
		policy := reverseProxyOf(t, cfg, key)["load_balancing"].(map[string]any)["selection_policy"].(map[string]any)
		return policy["cookie"].(string)
	}
	if a, b := cookieOf("a"), cookieOf("b"); a != "lb_8080" || b != "lb_8081" || a == b {
		t.Errorf("cookies %q and %q, want lb_8080 and lb_8081", a, b)
	}
	if cookieOf("a") != cookieOf("c") {
		t.Errorf("same port, other host: %q vs %q", cookieOf("a"), cookieOf("c"))
	}
	for _, key := range []string{"a", "b", "c"} {
		name := cookieOf(key)
		if !regexp.MustCompile(`^[a-z]+_[0-9]+$`).MatchString(name) || productName.MatchString(name) {
			t.Errorf("cookie name %q is not neutral", name)
		}
	}
}

// A config written by the previous version lists the copies itself; it is
// read like any other, and then replaced because it differs from what Render
// makes.
func TestParse_AcceptsAConfigThatListsTheCopies(t *testing.T) {
	cfg := `{"apps":{"http":{"servers":{"/x/a/state.json":{
		"listen":["tcp4/0.0.0.0:8765"],
		"routes":[{"handle":[
			{"handler":"headers","response":{"delete":["Server"],"deferred":true}},
			{"handler":"reverse_proxy","upstreams":[{"dial":"127.0.0.1:20000"}],
			 "load_balancing":{"try_duration":"15s","try_interval":"250ms"},"stream_close_delay":"12h"}]}]}}}}}`
	got, err := Parse([]byte(cfg))
	if err != nil || len(got) != 1 || got[0].Key != "/x/a/state.json" || got[0].Host != "0.0.0.0" || got[0].Port != 8765 {
		t.Fatalf("Parse = %+v, %v", got, err)
	}

	now, err := Render(testSettings, []App{{Key: "/x/a/state.json", Name: "a", Host: "0.0.0.0", Port: 8765}})
	if err != nil {
		t.Fatal(err)
	}
	nowV, _ := decodeConfig(now)
	oldV, _ := decodeConfig([]byte(cfg))
	if reflect.DeepEqual(nowV, oldV) {
		t.Error("the old shape equals the new one; it would never be replaced")
	}
}
