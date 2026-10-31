/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
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
	now := time.Date(2026, 10, 11, 19, 0, 0, 0, time.UTC)
	t.Cleanup(func() { statusNow = time.Now })
	statusNow = func() time.Time { return now }
	two := 2
	zero := 0
	resp := api.StatusResponse{
		BalancerMemory: 41 << 20,
		Apps: []api.AppStatus{
			{Name: "my-app", Port: 8080, Replicas: 1, WarmReplicas: 1, Copies: []api.CopyStatus{
				{Copy: 1, Role: "active", PID: 61429, Alive: true, Ready: true, StartedAt: started, MemoryBytes: 142 << 20, Sessions: &two},
				{Copy: 2, Role: "warm", PID: 61430, Alive: true, Ready: true, StartedAt: started.Add(time.Second), MemoryBytes: 139 << 20, Sessions: &zero},
			}},
			{Name: "flaky", Port: 8081, Copies: []api.CopyStatus{{Copy: 1, CrashCount: 3}}},
			{Name: "parked", Port: 8082, Stopped: true},
			{Name: "blocked", Port: 8083, Copies: []api.CopyStatus{{Copy: 1, PID: 7, Alive: true}},
				NotServed: "127.0.0.1:8083 is not available (taken) — pick a different port"},
			{Name: "taken", Port: 8084, NotStarted: "127.0.0.1:8084 is not available (taken) — pick a different port"},
			{Name: "rolling", Port: 8085, Replicas: 1, WarmReplicas: 1, Copies: []api.CopyStatus{
				{Copy: 1, Role: "active", PID: 11, Alive: true, Ready: true, Outdated: true},
				{Copy: 2, Role: "warm", PID: 12, Alive: true, Ready: true, Outdated: true},
				{Copy: 3, PID: 13, Alive: true},
			}},
			{Name: "draining", Port: 8086, Replicas: 1, WarmReplicas: 1, Copies: []api.CopyStatus{
				{Copy: 1, Role: "draining", PID: 21, Alive: true, Ready: true, Outdated: true, Sessions: &two, DrainEndsAt: now.Add(15*time.Minute - time.Second)},
				{Copy: 2, Role: "draining", PID: 22, Alive: true, Ready: true, Outdated: true, Sessions: &zero, DrainEndsAt: now.Add(15*time.Minute - time.Second)},
				{Copy: 3, Role: "active", PID: 23, Alive: true, Ready: true, Sessions: &two},
				{Copy: 4, PID: 24, Alive: true},
			}},
			{Name: "oneshot", Port: 8087, RestartPolicy: "never", Replicas: 2, Copies: []api.CopyStatus{
				{Copy: 1, Role: "active", PID: 31, Alive: true, Ready: true},
				{Copy: 2, PID: 32, GaveUp: true, StartedAt: started},
			}},
			{Name: "finished", Port: 8088, RestartPolicy: "on-failure", Copies: []api.CopyStatus{
				{Copy: 1, PID: 41, GaveUp: true, StartedAt: started},
			}},
		},
	}

	var out bytes.Buffer
	if err := writeStatus(&out, resp); err != nil {
		t.Fatal(err)
	}

	want := `NAME      COPY  ROLE      PORT  PID    STATUS         READY     MEM   SESSIONS  CRASHES  STARTED
my-app    1     active    8080  61429  alive          ready     142M  2         -        2026-10-11T18:20:21Z
my-app    2     warm      8080  61430  alive          ready     139M  0         -        2026-10-11T18:20:22Z
flaky     1     -         8081  0      crash-looping  -         -     -         3        -
parked    -     -         8082  0      stopped        -         -     -         -        -
blocked   1     -         8083  7      alive          starting  -     -         -        -
taken     -     -         8084  0      port-in-use    -         -     -         -        -
rolling   1     active    8085  11     alive          ready     -     -         -        -
rolling   2     warm      8085  12     alive          ready     -     -         -        -
rolling   3     -         8085  13     alive          starting  -     -         -        -
draining  1     draining  8086  21     alive          ready     -     2         -        -
draining  2     draining  8086  22     alive          ready     -     0         -        -
draining  3     active    8086  23     alive          ready     -     2         -        -
draining  4     -         8086  24     alive          starting  -     -         -        -
oneshot   1     active    8087  31     alive          ready     -     -         -        -
oneshot   2     -         8087  32     exited         -         -     -         -        2026-10-11T18:20:21Z
finished  1     -         8088  41     exited         -         -     -         -        2026-10-11T18:20:21Z

Memory: 281M in 11 R processes (2 warm, 2 draining), plus 41M for the load balancer.

blocked: its port is not served: 127.0.0.1:8083 is not available (taken) — pick a different port

taken: not started: 127.0.0.1:8084 is not available (taken) — pick a different port

rolling: new version starting in copy 3; copies 1 and 2 keep serving the previous version until it is ready, then drain

oneshot: copy 2 exited and restart-policy never leaves it down; apply or restart starts it again.

finished: copy 1 exited without an error and restart-policy on-failure leaves it down; apply or restart starts it again. Its port is closed.

draining: copy 4, the new spare, is starting; until it is ready the app has no spare.

draining: copies 1 and 2 are draining with 2 sessions; each stops when its sessions end, or in 15m at the latest.
`
	if out.String() != want {
		t.Errorf("table =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestRolloutNote(t *testing.T) {
	copyOf := func(n int, outdated, alive, ready bool) api.CopyStatus {
		return api.CopyStatus{Copy: n, Outdated: outdated, Alive: alive, Ready: ready}
	}
	skip := func(c api.CopyStatus) api.CopyStatus { c.SkipDrain = true; return c }
	draining := func(c api.CopyStatus) api.CopyStatus { c.Role = "draining"; return c }
	cases := []struct {
		name string
		app  api.AppStatus
		want string
	}{
		{"the new copy is starting", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, true, true, true), copyOf(2, false, true, false)}},
			"new version starting in copy 2; copy 1 keeps serving the previous version until it is ready, then drains"},
		{"two old, two new starting", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, true, true, true), copyOf(2, true, true, true), copyOf(3, false, true, false), copyOf(4, false, true, false)}},
			"new version starting in copies 3 and 4; copies 1 and 2 keep serving the previous version until it is ready, then drain"},
		{"with --now the old copies stop", api.AppStatus{Copies: []api.CopyStatus{skip(copyOf(1, true, true, true)), copyOf(2, false, true, false)}},
			"new version starting in copy 2; copy 1 keeps serving the previous version until it is ready, then stops"},
		{"with --now, several", api.AppStatus{Copies: []api.CopyStatus{skip(copyOf(1, true, true, true)), skip(copyOf(2, true, true, true)), copyOf(3, false, true, false)}},
			"new version starting in copy 3; copies 1 and 2 keep serving the previous version until it is ready, then stop"},
		{"the new copy is crash-looping", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, true, true, true), {Copy: 2, CrashCount: 3}}},
			"copy 1 keeps serving the previous version: copy 2 does not start (see shinyctl logs --copy 2)"},
		{"several old, several crash-looping", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, true, true, true), copyOf(2, true, true, true), {Copy: 3, CrashCount: 3}, {Copy: 4, CrashCount: 1}}},
			"copies 1 and 2 keep serving the previous version: copies 3 and 4 do not start (see shinyctl logs --copy 3)"},
		{"the new copy gave up", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, true, true, true), {Copy: 2, CrashCount: 1, GaveUp: true}}},
			"copy 1 keeps serving the previous version: copy 2 gave up starting (see shinyctl logs --copy 2; apply again once it is fixed)"},
		{"one new copy starting and one crash-looping", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, true, true, true), copyOf(2, true, true, true), copyOf(3, false, true, false), {Copy: 4, CrashCount: 2}}},
			"copies 1 and 2 keep serving the previous version: copy 4 does not start (see shinyctl logs --copy 4)"},
		{"one gave up and one crash-looping", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, true, true, true), {Copy: 2, CrashCount: 2}, {Copy: 3, CrashCount: 1, GaveUp: true}}},
			"copy 1 keeps serving the previous version: copies 2 and 3 do not start (see shinyctl logs --copy 2)"},
		{"a starting copy that has crashed before is still just starting", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, true, true, true), {Copy: 2, Alive: true, CrashCount: 2}}},
			"new version starting in copy 2; copy 1 keeps serving the previous version until it is ready, then drains"},
		{"no old copy serves", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, false, true, false), copyOf(2, false, true, false)}}, ""},
		{"old copies, nothing starting", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, true, true, true), copyOf(2, false, true, true)}}, ""},
		{"a draining copy is not serving", api.AppStatus{Copies: []api.CopyStatus{draining(copyOf(1, true, true, true)), copyOf(2, false, true, false)}}, ""},
	}
	for _, tc := range cases {
		if got := rolloutNote(tc.app); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDrainNote(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	n := func(i int) *int { return &i }
	copyOf := func(num int, sessions *int, left time.Duration) api.CopyStatus {
		return api.CopyStatus{Copy: num, Role: "draining", Alive: true, Ready: true, Sessions: sessions, DrainEndsAt: now.Add(left)}
	}
	cases := []struct {
		name string
		app  api.AppStatus
		want string
	}{
		{"one copy, no sessions", api.AppStatus{Copies: []api.CopyStatus{copyOf(2, n(0), 14*time.Minute)}},
			"copy 2 is draining with no sessions; it stops within seconds"},
		{"two copies, no sessions", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, n(0), 14*time.Minute), copyOf(2, n(0), 14*time.Minute)}},
			"copies 1 and 2 are draining with no sessions; they stop within seconds"},
		{"one copy, one session", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, n(1), 15*time.Minute)}},
			"copy 1 is draining with 1 session; it stops when it ends, or in 15m at the latest"},
		{"one copy, three sessions", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, n(3), 13*time.Minute+time.Second)}},
			"copy 1 is draining with 3 sessions; it stops when they end, or in 14m at the latest"},
		{"two copies, sessions", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, n(2), 14*time.Minute), copyOf(2, n(1), 13*time.Minute)}},
			"copies 1 and 2 are draining with 3 sessions; each stops when its sessions end, or in 14m at the latest"},
		{"sessions unknown", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, nil, 14*time.Minute)}},
			"copy 1 is draining; it stops when its sessions end, or in 14m at the latest"},
		{"sessions unknown, several", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, nil, 14*time.Minute), copyOf(2, n(0), 14*time.Minute)}},
			"copies 1 and 2 are draining; each stops when its sessions end, or in 14m at the latest"},
		{"the deadline has passed", api.AppStatus{Copies: []api.CopyStatus{copyOf(1, n(2), -time.Second)}},
			"copy 1 is draining with 2 sessions; it stops within seconds"},
		{"nothing drains", api.AppStatus{Copies: []api.CopyStatus{{Copy: 1, Role: "active", Alive: true}}}, ""},
		{"a dead draining copy is not counted", api.AppStatus{Copies: []api.CopyStatus{{Copy: 1, Role: "draining"}}}, ""},
	}
	for _, tc := range cases {
		if got := drainNote(tc.app, now); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFormatLeft(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Second:                           "1s",
		45 * time.Second:                      "45s",
		59*time.Second + 500*time.Millisecond: "60s",
		time.Minute:                           "1m",
		14*time.Minute + time.Second:          "15m",
		15 * time.Minute:                      "15m",
		time.Hour + 5*time.Minute:             "1h05m",
		2 * time.Hour:                         "2h",
	} {
		if got := formatLeft(d); got != want {
			t.Errorf("formatLeft(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestSpareNote(t *testing.T) {
	cases := []struct {
		name string
		app  api.AppStatus
		want string
	}{
		{"the new spare is starting after the switch", api.AppStatus{WarmReplicas: 1, Copies: []api.CopyStatus{
			{Copy: 1, Role: "draining", Alive: true, Ready: true, Outdated: true}, {Copy: 3, Role: "active", Alive: true, Ready: true}, {Copy: 4, Alive: true}}},
			"copy 4, the new spare, is starting; until it is ready the app has no spare"},
		{"no spares wanted", api.AppStatus{WarmReplicas: 0, Copies: []api.CopyStatus{
			{Copy: 1, Role: "draining", Alive: true, Ready: true, Outdated: true}, {Copy: 3, Role: "active", Alive: true, Ready: true}, {Copy: 4, Alive: true}}}, ""},
		{"nothing drains", api.AppStatus{WarmReplicas: 1, Copies: []api.CopyStatus{{Copy: 3, Role: "active", Alive: true, Ready: true}, {Copy: 4, Alive: true}}}, ""},
	}
	for _, tc := range cases {
		if got := spareNote(tc.app); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMemoryLine_Draining(t *testing.T) {
	two := 2
	active := api.CopyStatus{Copy: 3, Role: "active", Alive: true, Ready: true, MemoryBytes: 100 << 20, Sessions: &two}
	draining := api.CopyStatus{Copy: 1, Role: "draining", Alive: true, Ready: true, MemoryBytes: 100 << 20, Sessions: &two}
	for _, tc := range []struct {
		name   string
		copies []api.CopyStatus
		want   string
	}{
		{"only draining", []api.CopyStatus{active, draining}, "Memory: 200M in 2 R processes (1 draining)."},
		{"warm and draining", []api.CopyStatus{active, {Copy: 4, Role: "warm", Alive: true, Ready: true, MemoryBytes: 100 << 20}, draining}, "Memory: 300M in 3 R processes (1 warm, 1 draining)."},
		{"a dead draining copy is not counted", []api.CopyStatus{active, {Copy: 1, Role: "draining"}}, "Memory: 100M in 1 R process."},
	} {
		if got := memoryLine(api.StatusResponse{Apps: []api.AppStatus{{Name: "a", Copies: tc.copies}}}); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestExitedNote(t *testing.T) {
	gone := func(n int) api.CopyStatus { return api.CopyStatus{Copy: n, GaveUp: true} }
	up := func(n int) api.CopyStatus { return api.CopyStatus{Copy: n, Alive: true, Ready: true, Role: "active"} }
	cases := []struct {
		name string
		app  api.AppStatus
		want string
	}{
		{"never, one copy of several", api.AppStatus{RestartPolicy: "never", Copies: []api.CopyStatus{up(1), gone(2)}},
			"copy 2 exited and restart-policy never leaves it down; apply or restart starts it again."},
		{"never, two copies", api.AppStatus{RestartPolicy: "never", Copies: []api.CopyStatus{up(1), gone(2), gone(3)}},
			"copies 2 and 3 exited and restart-policy never leaves them down; apply or restart starts them again."},
		{"on-failure, a clean exit", api.AppStatus{RestartPolicy: "on-failure", Copies: []api.CopyStatus{up(1), gone(2)}},
			"copy 2 exited without an error and restart-policy on-failure leaves it down; apply or restart starts it again."},
		{"on-failure, two clean exits", api.AppStatus{RestartPolicy: "on-failure", Copies: []api.CopyStatus{up(1), gone(2), gone(3)}},
			"copies 2 and 3 exited without an error and restart-policy on-failure leaves them down; apply or restart starts them again."},
		{"every copy exited", api.AppStatus{RestartPolicy: "never", Copies: []api.CopyStatus{gone(1), gone(2)}},
			"copies 1 and 2 exited and restart-policy never leaves them down; apply or restart starts them again. Its port is closed."},
		{"a daemon that doesn't send the policy", api.AppStatus{Copies: []api.CopyStatus{gone(1)}},
			"copy 1 exited and the restart policy leaves it down; apply or restart starts it again. Its port is closed."},
		{"nothing exited", api.AppStatus{RestartPolicy: "never", Copies: []api.CopyStatus{up(1)}}, ""},
		{"a crash-looping copy is not an exit", api.AppStatus{RestartPolicy: "always", Copies: []api.CopyStatus{up(1), {Copy: 2, CrashCount: 3}}}, ""},
		{"a rollout note names it already", api.AppStatus{RestartPolicy: "never", Copies: []api.CopyStatus{
			{Copy: 1, Alive: true, Ready: true, Outdated: true, Role: "active"}, gone(2)}}, ""},
	}
	for _, c := range cases {
		if got := exitedNote(c.app); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
