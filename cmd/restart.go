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

var restartManifestFile string

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Stop an app's processes and start them again from the manifest as it is now",

	RunE: func(cmd *cobra.Command, args []string) error {
		spec, err := manifest.Load(restartManifestFile)
		if err != nil {
			return err
		}

		if err := manifest.CheckPathCharacters(spec); err != nil {
			return fmt.Errorf("invalid manifest: %w", err)
		}
		if err := manifest.Validate(spec); err != nil {
			return fmt.Errorf("invalid manifest: %w", err)
		}

		if err := ensureAPIRunning(); err != nil {
			return fmt.Errorf("shinyctl-api: %w", err)
		}

		socketPath, err := process.SocketPath()
		if err != nil {
			return err
		}

		resp, err := api.Restart(socketPath, spec)
		if err != nil {
			return err
		}

		fmt.Println(resp.Message)
		return nil
	},
}

func init() {
	restartCmd.Flags().StringVarP(
		&restartManifestFile,
		"file",
		"f",
		"",
		"path to the app manifest YAML file",
	)

	if err := restartCmd.MarkFlagRequired("file"); err != nil {
		panic(err)
	}

	rootCmd.AddCommand(restartCmd)
}
