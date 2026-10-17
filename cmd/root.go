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

  shinyctl init                        scaffold a manifest to fill in
  shinyctl apply -f shinyctl.yaml      declare an app, start/reconcile it
  shinyctl status                      see what's running right now
  shinyctl stop -f shinyctl.yaml       take it down and keep it down
  shinyctl delete -f shinyctl.yaml     stop it and forget it entirely`,
}

func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}
