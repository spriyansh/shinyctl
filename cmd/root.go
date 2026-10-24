/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "shinyctl",
	Short: "A small, Kubernetes-inspired supervisor for R Shiny apps on one machine",
	Long: `shinyctl keeps R Shiny apps running on a single machine, no root required.

Declare an app in a YAML manifest, apply it, and shinyctl starts it and
restarts it if it dies. A per-user background process (shinyctl-api)
watches every applied app; it is started automatically by the first apply.
A built-in load balancer listens on each app's port, so requests wait while
an app restarts instead of failing.

  shinyctl init                        scaffold a manifest to fill in
  shinyctl apply -f shinyctl.yaml      declare an app, start/reconcile it
  shinyctl status                      see what's running right now
  shinyctl stop -f shinyctl.yaml       take it down and keep it down
  shinyctl delete -f shinyctl.yaml     stop it and forget it entirely
  shinyctl restart -f shinyctl.yaml    pick up code changes
  shinyctl logs -f shinyctl.yaml       print the app's log (--follow to keep watching)
  shinyctl startup                     print a systemd unit that starts shinyctl-api at boot`,
}

func init() {
	// Once the arguments and flags have parsed, an error is the command's
	// own (a refused move, a daemon that isn't there), and cobra's usage
	// text under it only buries the message.
	rootCmd.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		cmd.SilenceUsage = true
	}
}

func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}
