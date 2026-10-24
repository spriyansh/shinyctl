/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"os"
	"regexp"
	"testing"
)

// TestREADMEHasNoProductName: the README describes a built-in load
// balancer and does not say what it is built on.
func TestREADMEHasNoProductName(t *testing.T) {
	data, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if loc := regexp.MustCompile(`(?i)caddy`).FindIndex(data); loc != nil {
		t.Errorf("README.md names the software behind the load balancer at byte %d", loc[0])
	}
}

// TestREADMEStopPattern: the pkill line in the README that stops everything
// matches the command lines of shinyctl-api and the load balancer, as shinyctl
// starts them, and nothing else of shinyctl's.
func TestREADMEStopPattern(t *testing.T) {
	data, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`pkill -u "\$USER" -f '([^']*proxy --config[^']*)'`).FindSubmatch(data)
	if m == nil {
		t.Fatal("README.md has no pkill line for the load balancer")
	}
	pattern := regexp.MustCompile(string(m[1]))

	for _, cmdline := range []string{
		"/home/me/go/bin/shinyctl serve",
		"shinyctl serve",
		"/home/me/go/bin/shinyctl proxy --config /home/me/.shinyctl/proxy/config.json",
	} {
		if !pattern.MatchString(cmdline) {
			t.Errorf("pattern does not match %q", cmdline)
		}
	}
	for _, cmdline := range []string{
		"/home/me/go/bin/shinyctl serve -d",
		"/home/me/go/bin/shinyctl status",
		"/home/me/go/bin/shinyctl apply -f /home/me/lab/shinyctl.yaml",
		"/home/me/go/bin/shinyctl logs -f x.yaml --follow",
		"vim shinyctl serve.txt",
		"/home/me/bin/notshinyctl serve",
		"/usr/lib/R/bin/exec/R --no-echo --no-restore -e shiny::runApp(\"/home/me/shinyctl proxy --config\")",
	} {
		if pattern.MatchString(cmdline) {
			t.Errorf("pattern matches %q", cmdline)
		}
	}
}
