/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestErrorsDoNotPrintUsage: a command that fails after its arguments parsed
// (a refused move, a manifest that can't be read) shows its error and not
// the help text under it.
func TestErrorsDoNotPrintUsage(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"stop", "-f", "/nonexistent/shinyctl.yaml"})
	defer func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	}()

	if err := rootCmd.Execute(); err == nil {
		t.Fatal("stop of a missing manifest succeeded")
	}
	if strings.Contains(out.String(), "Usage:") {
		t.Errorf("output includes the usage text:\n%s", out.String())
	}
}
