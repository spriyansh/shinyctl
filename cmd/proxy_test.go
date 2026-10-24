/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"regexp"
	"testing"
)

// TestProxyCommandHidden: the load balancer's process is an internal detail
// of shinyctl-api. It must not show up in help, and no help text may name
// the software behind it.
func TestProxyCommandHidden(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	defer rootCmd.SetOut(nil)
	defer rootCmd.SetErr(nil)

	if err := rootCmd.Help(); err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?m)^\s+proxy\s`).MatchString(out.String()) {
		t.Errorf("root help lists the proxy command:\n%s", out.String())
	}

	name := regexp.MustCompile(`(?i)caddy`)
	for _, text := range []string{out.String(), proxyCmd.Short, proxyCmd.Long, rootCmd.Long} {
		if name.MatchString(text) {
			t.Errorf("help text names the software: %q", text)
		}
	}
}
