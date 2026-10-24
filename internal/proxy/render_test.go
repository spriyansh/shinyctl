package proxy

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

var testSettings = SettingsFor("/home/me/.shinyctl/proxy")

var testApps = []App{
	{Key: "/home/me/apps/explorer/state.json", Name: "explorer", Group: "lab", Host: "0.0.0.0", Port: 8765, Upstreams: []string{"127.0.0.1:20000"}},
	{Key: "/home/me/apps/maps/state.json", Name: "maps", Group: "lab", Host: "127.0.0.1", Port: 8080, Upstreams: []string{"127.0.0.1:20001"}},
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
		cfg, err := Render(testSettings, []App{{Key: "k", Host: host, Port: 8080, Upstreams: []string{"127.0.0.1:20000"}}})
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
		// No passive checks while there is one copy: they would keep it
		// marked down after every restart.
		if _, ok := rp["health_checks"]; ok {
			t.Errorf("%s: has health_checks, which delay every restart by their fail_duration", key)
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
		cfg, err := Render(testSettings, []App{{Key: "k", Host: host, Port: 8080, Upstreams: []string{"127.0.0.1:20000"}}})
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
