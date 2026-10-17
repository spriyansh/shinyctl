/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version is overridden at build time via -ldflags by GoReleaser
// (-X github.com/spriyansh/shinyctl/cmd.Version={{.Version}}); a plain
// `go build`/`go install` leaves it at "dev".
var Version = "dev"

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the shinyctl version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(Version)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
