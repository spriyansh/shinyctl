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

var manifestFile string

// ensureAPIRunning starts shinyctl-api if it isn't running; a variable so a
// test can tell whether a command got as far as asking for it.
var ensureAPIRunning = process.EnsureAPIRunning

var applyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Reconcile a shiny app to the state declared in a YAML manifest",

	RunE: func(cmd *cobra.Command, args []string) error {
		spec, err := manifest.Load(manifestFile)
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

		resp, err := api.Apply(socketPath, spec)
		if err != nil {
			return err
		}

		fmt.Println(resp.Message)
		return nil
	},
}

func init() {
	applyCmd.Flags().StringVarP(
		&manifestFile,
		"file",
		"f",
		"",
		"path to the app manifest YAML file",
	)

	if err := applyCmd.MarkFlagRequired("file"); err != nil {
		panic(err)
	}

	rootCmd.AddCommand(applyCmd)
}
