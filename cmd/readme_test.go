/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"os"
	"regexp"
	"strings"
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

// TestREADMEDescribesCopies: the manifest example has the settings for
// copies, and the line that said one copy was all there is is gone.
func TestREADMEDescribesCopies(t *testing.T) {
	data, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"warm-replicas", "lb-policy", "lb_<port>", "--copy N", "port-in-use", "drain-timeout", "--now", "draining",
		"exited", "THIRD_PARTY_NOTICES", "host: 127.0.0.1", "proxy_http_version 1.1", "Upgrade $http_upgrade", "proxy_read_timeout",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("README.md does not mention %q", want)
		}
	}
	for _, gone := range []string{"1 is the only accepted value", "One process per app", "one copy at a time", "Restarts go one copy at a time", "There are no prebuilt binaries yet"} {
		if strings.Contains(string(data), gone) {
			t.Errorf("README.md still says %q", gone)
		}
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

// TestREADMEArchiveNameMatchesTheRelease: the archive the README tells a
// reader to download, and the folder it unpacks into, are what
// .goreleaser.yaml makes for version 0.1.0 on linux/arm64.
func TestREADMEArchiveNameMatchesTheRelease(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	conf, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*name_template: "(shinyctl_[^"]*)"`).FindSubmatch(conf)
	if m == nil {
		t.Fatal(".goreleaser.yaml has no archive name_template")
	}
	name := strings.NewReplacer("{{ .Version }}", "0.1.0", "{{ .Os }}", "linux", "{{ .Arch }}", "arm64").Replace(string(m[1]))
	if strings.Contains(name, "{{") {
		t.Fatalf("the name template %q has a part this test doesn't fill in", m[1])
	}
	if !regexp.MustCompile(`(?m)^\s*wrap_in_directory: true`).Match(conf) {
		t.Error(".goreleaser.yaml does not wrap the archive in a folder, but the README unpacks into one")
	}
	for _, want := range []string{
		name + ".tar.gz",
		"tar -xzf " + name + ".tar.gz",
		"cp " + name + "/shinyctl ~/.local/bin/",
		"releases/download/v0.1.0/" + name + ".tar.gz",
	} {
		if !strings.Contains(string(readme), want) {
			t.Errorf("README.md does not contain %q", want)
		}
	}
}
