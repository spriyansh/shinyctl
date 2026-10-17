/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
)

var deleteManifestFile string

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Stop an app and forget it entirely — gone from status, state file removed",

	// Unlike apply/stop, this deliberately skips manifest.Validate: its
	// filesystem checks (entrypoint exists, Rscript on PATH) are exactly
	// what you can't satisfy when the reason you're deleting is that the
	// app's files are already gone. Only the fields delete actually uses
	// are required.
	RunE: func(cmd *cobra.Command, args []string) error {
		spec, err := manifest.Load(deleteManifestFile)
		if err != nil {
			return err
		}

		if spec.AppName == "" || spec.AppStateFile == "" {
			return fmt.Errorf("invalid manifest: app-name and app-state-file are required")
		}

		if err := process.EnsureAPIRunning(); err != nil {
			return fmt.Errorf("shinyctl-api: %w", err)
		}

		socketPath, err := process.SocketPath()
		if err != nil {
			return err
		}

		resp, err := api.Delete(socketPath, spec)
		if err != nil {
			return err
		}

		fmt.Println(resp.Message)
		return nil
	},
}

func init() {
	deleteCmd.Flags().StringVarP(
		&deleteManifestFile,
		"file",
		"f",
		"",
		"path to the app manifest YAML file",
	)

	if err := deleteCmd.MarkFlagRequired("file"); err != nil {
		panic(err)
	}

	rootCmd.AddCommand(deleteCmd)
}
