/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"github.com/spf13/cobra"
	"github.com/spriyansh/shinyctl/internal/proxy"
)

var (
	proxyConfig string
	proxyAfter  int
)

// proxyCmd is the load balancer's process. shinyctl-api starts it and
// keeps it running; it is hidden because nobody runs it by hand.
var proxyCmd = &cobra.Command{
	Use:    "proxy --config <file>",
	Short:  "Run the load balancer in front of the apps (started by shinyctl-api)",
	Hidden: true,
	Args:   cobra.NoArgs,

	// A second instance is an ordinary refusal, not a usage mistake: print
	// the error and nothing else.
	SilenceUsage: true,

	RunE: func(cmd *cobra.Command, args []string) error {
		return proxy.Run(proxyConfig, proxyAfter)
	},
}

func init() {
	proxyCmd.Flags().StringVar(&proxyConfig, "config", "", "the load balancer's settings file (absolute path)")
	proxyCmd.MarkFlagRequired("config")
	proxyCmd.Flags().IntVar(&proxyAfter, "after", 0, "the pid of the load balancer this one replaces: serve next to it first")

	rootCmd.AddCommand(proxyCmd)
}
