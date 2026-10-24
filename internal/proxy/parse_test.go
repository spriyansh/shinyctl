package proxy

import "testing"

func TestParse_RoundTripsRender(t *testing.T) {
	apps := []App{
		testApp("/x/a/state.json", "a", "0.0.0.0", 8765),
		{Key: "/x/b/state.json", Name: "b", Group: "g", Host: "::1", Port: 8080},
	}
	cfg, err := Render(testSettings, apps)
	if err != nil {
		t.Fatal(err)
	}

	got, err := Parse(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(apps) {
		t.Fatalf("Parse returned %d apps, want %d", len(got), len(apps))
	}
	for i, want := range apps {
		g := got[i]
		if g.Key != want.Key || g.Host != want.Host || g.Port != want.Port {
			t.Errorf("app %d = %+v, want the key, host and port of %+v", i, g, want)
		}
	}
}

func TestParse_NoApps(t *testing.T) {
	cfg, err := Render(testSettings, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(cfg)
	if err != nil || len(got) != 0 {
		t.Errorf("Parse(bootstrap) = %v, %v, want no apps and no error", got, err)
	}
}

func TestParse_UnknownShape(t *testing.T) {
	for name, cfg := range map[string]string{
		"not json":         `nope`,
		"two listen":       `{"apps":{"http":{"servers":{"k":{"listen":["tcp4/1.2.3.4:1","tcp4/1.2.3.4:2"]}}}}}`,
		"no prefix":        `{"apps":{"http":{"servers":{"k":{"listen":[":80"]}}}}}`,
		"no reverse_proxy": `{"apps":{"http":{"servers":{"k":{"listen":["tcp4/1.2.3.4:80"],"routes":[{"handle":[{"handler":"static_response"}]}]}}}}}`,
	} {
		if _, err := Parse([]byte(cfg)); err == nil {
			t.Errorf("%s: Parse succeeded, want an error", name)
		}
	}
}
