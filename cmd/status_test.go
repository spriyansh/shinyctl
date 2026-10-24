/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
)

func TestFormatMem(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "-"},
		{-1, "-"},
		{512 << 10, "512K"},
		{1023 << 10, "1023K"},
		{1 << 20, "1M"},
		{142 << 20, "142M"},
		{1048575 << 10, "1.0G"}, // rounds to 1024M, which is shown as 1.0G
		{3 << 29, "1.5G"},
	}

	for _, c := range cases {
		if got := formatMem(c.bytes); got != c.want {
			t.Errorf("formatMem(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}

func TestWriteStatus(t *testing.T) {
	started := time.Date(2026, 10, 11, 18, 20, 21, 0, time.UTC)
	two := 2
	zero := 0
	resp := api.StatusResponse{
		BalancerRSS: 41 << 20,
		Apps: []api.AppStatus{
			{Name: "my-app", Port: 8080, Replicas: 1, WarmReplicas: 1, Copies: []api.CopyStatus{
				{Copy: 1, Role: "active", PID: 61429, Alive: true, Ready: true, StartedAt: started, RSSBytes: 142 << 20, Sessions: &two},
				{Copy: 2, Role: "warm", PID: 61430, Alive: true, Ready: true, StartedAt: started.Add(time.Second), RSSBytes: 139 << 20, Sessions: &zero},
			}},
			{Name: "flaky", Port: 8081, Copies: []api.CopyStatus{{Copy: 1, CrashCount: 3}}},
			{Name: "parked", Port: 8082, Stopped: true},
			{Name: "blocked", Port: 8083, Copies: []api.CopyStatus{{Copy: 1, PID: 7, Alive: true}},
				NotServed: "127.0.0.1:8083 is not available (taken) — pick a different port"},
			{Name: "taken", Port: 8084, NotStarted: "127.0.0.1:8084 is not available (taken) — pick a different port"},
			{Name: "rolling", Port: 8085, Copies: []api.CopyStatus{
				{Copy: 1, Role: "active", PID: 11, Alive: true, Ready: true, Outdated: true},
				{Copy: 2, PID: 12, Alive: true, Outdated: true},
				{Copy: 3, PID: 13, Alive: true, Ready: true},
			}},
		},
	}

	var out bytes.Buffer
	if err := writeStatus(&out, resp); err != nil {
		t.Fatal(err)
	}

	want := `NAME     COPY  ROLE    PORT  PID    STATUS         READY     MEM   SESSIONS  CRASHES  STARTED
my-app   1     active  8080  61429  alive          ready     142M  2         -        2026-10-11T18:20:21Z
my-app   2     warm    8080  61430  alive          ready     139M  0         -        2026-10-11T18:20:22Z
flaky    1     -       8081  0      crash-looping  -         -     -         3        -
parked   -     -       8082  0      stopped        -         -     -         -        -
blocked  1     -       8083  7      alive          starting  -     -         -        -
taken    -     -       8084  0      port-in-use    -         -     -         -        -
rolling  1     active  8085  11     alive          ready     -     -         -        -
rolling  2     -       8085  12     alive          starting  -     -         -        -
rolling  3     -       8085  13     alive          ready     -     -         -        -

Memory: 281M in 6 R processes (1 warm), plus 41M for the load balancer.

blocked: its port is not served: 127.0.0.1:8083 is not available (taken) — pick a different port

taken: not started: 127.0.0.1:8084 is not available (taken) — pick a different port

rolling: restarting one copy at a time; copies 1 and 2 still run the previous version
`
	if out.String() != want {
		t.Errorf("table =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestKeptOverNote(t *testing.T) {
	copyOf := func(n int, outdated, alive, ready bool) api.CopyStatus {
		return api.CopyStatus{Copy: n, Outdated: outdated, Alive: alive, Ready: ready}
	}
	cases := []struct {
		name string
		app  api.AppStatus
		want string
	}{
		{"one wanted, the new copy is starting", api.AppStatus{Replicas: 1, Copies: []api.CopyStatus{copyOf(1, true, true, true), copyOf(2, false, true, false)}},
			"copy 1 stays until copy 2 is ready (1 copy wanted)"},
		{"one wanted, the new copy crashed", api.AppStatus{Replicas: 1, Copies: []api.CopyStatus{copyOf(1, true, true, true), copyOf(2, false, false, false)}},
			"copy 1 stays until copy 2 is ready (1 copy wanted)"},
		{"two wanted, two old, two new", api.AppStatus{Replicas: 1, WarmReplicas: 1, Copies: []api.CopyStatus{copyOf(1, true, true, true), copyOf(2, true, true, true), copyOf(3, false, true, false), copyOf(4, false, true, false)}},
			"copies 1 and 2 stay until copies 3 and 4 are ready (2 copies wanted)"},
		{"one wanted, the new copy is crash-looping", api.AppStatus{Replicas: 1, Copies: []api.CopyStatus{copyOf(1, true, true, true), {Copy: 2, CrashCount: 3}}},
			"copy 1 keeps serving the previous version: copy 2 does not start (see shinyctl logs --copy 2)"},
		{"one wanted, the new copy gave up", api.AppStatus{Replicas: 1, Copies: []api.CopyStatus{copyOf(1, true, true, true), {Copy: 2, CrashCount: 1, GaveUp: true}}},
			"copy 1 keeps serving the previous version: copy 2 gave up starting (see shinyctl logs --copy 2; apply again once it is fixed)"},
		{"two wanted, one new copy starting and one crash-looping", api.AppStatus{Replicas: 1, WarmReplicas: 1, Copies: []api.CopyStatus{copyOf(1, true, true, true), copyOf(2, true, true, true), copyOf(3, false, true, false), {Copy: 4, CrashCount: 2}}},
			"copies 1 and 2 keep serving the previous version: copy 4 does not start (see shinyctl logs --copy 4)"},
		{"a starting copy that has crashed before is still just starting", api.AppStatus{Replicas: 1, Copies: []api.CopyStatus{copyOf(1, true, true, true), {Copy: 2, Alive: true, CrashCount: 2}}},
			"copy 1 stays until copy 2 is ready (1 copy wanted)"},
		{"the number wanted: nothing kept over", api.AppStatus{Replicas: 1, WarmReplicas: 1, Copies: []api.CopyStatus{copyOf(1, true, true, true), copyOf(2, false, true, false)}}, ""},
		{"over the number, but no rollout", api.AppStatus{Replicas: 1, Copies: []api.CopyStatus{copyOf(1, false, true, true), copyOf(2, false, true, true)}}, ""},
		{"over the number, the new copy is ready", api.AppStatus{Replicas: 1, Copies: []api.CopyStatus{copyOf(1, true, true, true), copyOf(2, false, true, true)}}, ""},
	}
	for _, tc := range cases {
		if got := keptOverNote(tc.app); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}

	var out bytes.Buffer
	app := api.AppStatus{Name: "app", Port: 8080, Replicas: 1, Copies: []api.CopyStatus{
		{Copy: 1, Role: "active", PID: 1, Alive: true, Ready: true, Outdated: true},
		{Copy: 2, PID: 2, Alive: true},
	}}
	if err := writeStatus(&out, api.StatusResponse{Apps: []api.AppStatus{app}}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"app: restarting one copy at a time; copy 1 still runs the previous version\n", "app: copy 1 stays until copy 2 is ready (1 copy wanted)\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, out.String())
		}
	}
}
