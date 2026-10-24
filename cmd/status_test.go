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
	apps := []api.AppStatus{
		{Name: "my-app", Port: 8080, PID: 61429, Alive: true, Ready: true, StartedAt: started, RSSBytes: 142 << 20},
		{Name: "flaky", Port: 8081, PID: 0, CrashCount: 3},
		{Name: "parked", Port: 8082, Stopped: true},
		{Name: "blocked", Port: 8083, Alive: true, NotServed: "127.0.0.1:8083 is not available (taken) — pick a different port"},
	}

	var out bytes.Buffer
	if err := writeStatus(&out, apps); err != nil {
		t.Fatal(err)
	}

	want := `NAME     PORT  PID    STATUS         READY     MEM   CRASHES  STARTED
my-app   8080  61429  alive          ready     142M  -        2026-10-11T18:20:21Z
flaky    8081  0      crash-looping  -         -     3        -
parked   8082  0      stopped        -         -     -        -
blocked  8083  0      alive          starting  -     -        -

blocked: its port is not served: 127.0.0.1:8083 is not available (taken) — pick a different port
`
	if out.String() != want {
		t.Errorf("table =\n%s\nwant\n%s", out.String(), want)
	}
}
