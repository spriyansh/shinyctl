package proxy

import (
	"slices"
	"testing"
)

func testApp(key, name, host string, port int) App {
	return App{Key: key, Name: name, Group: "g", Host: host, Port: port}
}

func TestPlan(t *testing.T) {
	cases := []struct {
		name     string
		apps     []App
		loaded   []App
		wantKeep []string
		wantLeft map[string]string
	}{
		{
			name:     "disjoint apps are kept",
			apps:     []App{testApp("b", "b", "127.0.0.1", 8081), testApp("a", "a", "127.0.0.1", 8080)},
			wantKeep: []string{"a", "b"},
		},
		{
			name:     "same address: the lower key keeps it",
			apps:     []App{testApp("b", "b", "127.0.0.1", 9000), testApp("a", "a", "127.0.0.1", 9000)},
			wantKeep: []string{"a"},
			wantLeft: map[string]string{"b": `127.0.0.1:9000 is already used by app "a" (app-group "g") — pick a different port`},
		},
		{
			name:     "the IPv4 wildcard overlaps a specific address",
			apps:     []App{testApp("a", "a", "0.0.0.0", 9000), testApp("b", "b", "127.0.0.1", 9000)},
			wantKeep: []string{"a"},
			wantLeft: map[string]string{"b": `127.0.0.1:9000 is already used by app "a" (app-group "g") — pick a different port`},
		},
		{
			name:     "the IPv4 wildcard does not overlap an IPv6 address",
			apps:     []App{testApp("a", "a", "0.0.0.0", 9000), testApp("b", "b", "::1", 9000)},
			wantKeep: []string{"a", "b"},
		},
		{
			name:     "the IPv6 wildcard overlaps a specific IPv6 address",
			apps:     []App{testApp("a", "a", "::", 9000), testApp("b", "b", "::1", 9000)},
			wantKeep: []string{"a"},
			wantLeft: map[string]string{"b": `[::1]:9000 is already used by app "a" (app-group "g") — pick a different port`},
		},
		{
			name:     "an app already served keeps its address against a lower key",
			apps:     []App{testApp("a", "a", "127.0.0.1", 9000), testApp("b", "b", "127.0.0.1", 9000)},
			loaded:   []App{testApp("b", "b", "127.0.0.1", 9000)},
			wantKeep: []string{"b"},
			wantLeft: map[string]string{"a": `127.0.0.1:9000 is already used by app "b" (app-group "g") — pick a different port`},
		},
		{
			name:     "a served app that moved gets no priority at its new address",
			apps:     []App{testApp("a", "a", "127.0.0.1", 9000), testApp("b", "b", "127.0.0.1", 9000)},
			loaded:   []App{testApp("b", "b", "127.0.0.1", 9001)},
			wantKeep: []string{"a"},
			wantLeft: map[string]string{"b": `127.0.0.1:9000 is already used by app "a" (app-group "g") — pick a different port`},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			keep, left := Plan(c.apps, c.loaded)

			var keys []string
			for _, a := range keep {
				keys = append(keys, a.Key)
			}
			if !slices.Equal(keys, c.wantKeep) {
				t.Errorf("keep = %v, want %v", keys, c.wantKeep)
			}
			if len(left) != len(c.wantLeft) {
				t.Errorf("left = %v, want %v", left, c.wantLeft)
			}
			for key, want := range c.wantLeft {
				if got := left[key]; got == nil || got.Error() != want {
					t.Errorf("left[%q] = %v, want %q", key, got, want)
				}
			}
		})
	}
}
