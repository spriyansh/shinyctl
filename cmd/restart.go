/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
)

var restartManifestFile string

// restartNow is --now: the old copies stop as soon as the new ones are ready.
var restartNow bool

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Start an app's copies again from the manifest as it is now, without cutting off visitors",

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

		resp, err := api.Restart(socketPath, spec, api.Options{Now: restartNow})
		if err != nil {
			return err
		}

		fmt.Println(resp.Message)
		warnIfLimitIgnored(os.Stderr, spec, resp)
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

	restartCmd.Flags().BoolVar(
		&restartNow,
		"now",
		false,
		"stop the old copies as soon as the new ones are ready, without waiting for their visitors (also ends any drain in progress)",
	)

	if err := restartCmd.MarkFlagRequired("file"); err != nil {
		panic(err)
	}

	rootCmd.AddCommand(restartCmd)
}
