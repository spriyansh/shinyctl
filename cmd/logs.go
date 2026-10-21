/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spriyansh/shinyctl/internal/logtail"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
)

// logsPollInterval is how often --follow checks the log for new lines.
const logsPollInterval = 250 * time.Millisecond

var (
	logsManifestFile string
	logsFollow       bool
	logsLines        int
	logsAPI          bool
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Print an app's log, optionally following it as it grows",

	RunE: func(cmd *cobra.Command, args []string) error {
		if logsLines < 0 {
			return fmt.Errorf("--lines must be 0 or more")
		}

		path, err := logsPath(logsManifestFile, logsAPI)
		if err != nil {
			return err
		}

		// Only a follow needs Ctrl-C to end normally. A plain print keeps
		// the default SIGINT behaviour, so it can still be interrupted
		// while reading a very large file.
		ctx := cmd.Context()
		if logsFollow {
			var stop context.CancelFunc
			ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
		}

		return printLogs(ctx, os.Stdout, path, logsLines, logsFollow, logsPollInterval)
	},
}

// logsPath returns the log file to read: shinyctl-api's own with api, or
// the app named by the manifest's app-group and app-name.
//
// Like delete and status, this deliberately skips manifest.Validate and
// never starts shinyctl-api: the log is just a file, and the reason to
// read it is often that the entrypoint is gone or the app won't start. Only
// the two fields the log's name comes from are required.
func logsPath(manifestFile string, api bool) (string, error) {
	if api {
		return process.LogPath()
	}

	spec, err := manifest.Load(manifestFile)
	if err != nil {
		return "", err
	}

	if spec.AppGroup == "" || spec.AppName == "" {
		return "", fmt.Errorf("invalid manifest: app-group and app-name are required")
	}

	return process.AppLogPath(spec)
}

// printLogs writes the last lines lines of the log at path to w and, when
// follow is set, keeps writing what is appended until ctx is cancelled.
func printLogs(ctx context.Context, w io.Writer, path string, lines int, follow bool, interval time.Duration) error {
	offset, err := logtail.Last(path, lines, w)
	if err == nil && follow {
		err = logtail.Follow(ctx, path, offset, w, interval)
	}

	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no log yet at %s", path)
	}
	return err
}

func init() {
	logsCmd.Flags().StringVarP(
		&logsManifestFile,
		"file",
		"f",
		"",
		"path to the app manifest YAML file",
	)

	// No -f shorthand here: -f means --file in every other command.
	logsCmd.Flags().BoolVar(
		&logsFollow,
		"follow",
		false,
		"keep printing new lines as they are written, until Ctrl-C",
	)

	logsCmd.Flags().IntVarP(
		&logsLines,
		"lines",
		"n",
		100,
		"number of lines to print from the end of the log",
	)

	logsCmd.Flags().BoolVar(
		&logsAPI,
		"api",
		false,
		"show shinyctl-api's own log instead of an app's",
	)

	logsCmd.MarkFlagsOneRequired("file", "api")
	logsCmd.MarkFlagsMutuallyExclusive("file", "api")

	rootCmd.AddCommand(logsCmd)
}
