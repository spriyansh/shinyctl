/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"sync"
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
	logsCopy         int
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Print an app's log, optionally following it as it grows",
	Long: `Print the end of an app's log. An app with several copies has a log for each:
without --copy every copy's log is printed under a header, and with --follow the
lines of all of them are shown, each starting with the copy it came from. Logs
of copies started after --follow began, as after a redeploy, are not picked up:
run it again.`,

	RunE: func(cmd *cobra.Command, args []string) error {
		if logsLines < 0 {
			return fmt.Errorf("--lines must be 0 or more")
		}
		if cmd.Flags().Changed("copy") && logsAPI {
			return fmt.Errorf("--copy goes with --file, not --api")
		}
		if logsCopy < 0 {
			return fmt.Errorf("--copy must be 0 (every copy) or more")
		}

		logs, err := logsTargets(logsManifestFile, logsAPI, logsCopy)
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

		if len(logs) == 1 {
			return printLogs(ctx, os.Stdout, logs[0].Path, logsLines, logsFollow, logsPollInterval)
		}
		return printCopyLogs(ctx, os.Stdout, logs, logsLines, logsFollow, logsPollInterval)
	},
}

// logsSpec loads the manifest for logs.
//
// Like delete and status, this deliberately skips manifest.Validate and
// never starts shinyctl-api: the log is just a file, and the reason to
// read it is often that the entrypoint is gone or the app won't start. Only
// the two fields the logs' names come from are required.
func logsSpec(manifestFile string) (manifest.AppSpec, error) {
	spec, err := manifest.Load(manifestFile)
	if err != nil {
		return spec, err
	}
	if spec.AppGroup == "" || spec.AppName == "" {
		return spec, fmt.Errorf("invalid manifest: app-group and app-name are required")
	}
	return spec, nil
}

// logsTargets lists the log files to read: shinyctl-api's own with api; copy
// n's with n > 0; otherwise every copy the app has logged, or copy 1's path
// (which is the log of an app with one copy, and "no log yet" if it has
// none) when there is not more than one.
func logsTargets(manifestFile string, api bool, copy int) ([]process.CopyLog, error) {
	if api {
		path, err := process.LogPath()
		if err != nil {
			return nil, err
		}
		return []process.CopyLog{{Path: path}}, nil
	}

	spec, err := logsSpec(manifestFile)
	if err != nil {
		return nil, err
	}
	if copy > 0 {
		path, err := process.CopyLogPath(spec, copy)
		if err != nil {
			return nil, err
		}
		return []process.CopyLog{{Copy: copy, Path: path}}, nil
	}

	logs, err := process.AppLogPaths(spec)
	if err != nil {
		return nil, err
	}
	if len(logs) <= 1 {
		path, err := process.AppLogPath(spec)
		if err != nil {
			return nil, err
		}
		return []process.CopyLog{{Copy: 1, Path: path}}, nil
	}
	return logs, nil
}

// printCopyLogs prints the end of each copy's log under a header, and with
// follow keeps printing what the copies add, each line starting with its
// copy. Whole lines only, one at a time, so the output of two copies is never
// mixed within a line.
func printCopyLogs(ctx context.Context, w io.Writer, logs []process.CopyLog, lines int, follow bool, interval time.Duration) error {
	offsets := make([]int64, len(logs))
	for i, l := range logs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "==> copy %d <==\n", l.Copy)
		offset, err := logtail.Last(l.Path, lines, w)
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no log yet at %s", l.Path)
		}
		if err != nil {
			return err
		}
		offsets[i] = offset
	}
	if !follow {
		return nil
	}

	var mu sync.Mutex
	errs := make(chan error, len(logs))
	for i, l := range logs {
		go func() {
			pw := &prefixWriter{mu: &mu, out: w, prefix: fmt.Sprintf("[copy %d] ", l.Copy)}
			err := logtail.Follow(ctx, l.Path, offsets[i], pw, interval)
			pw.Flush() // a last line without its newline is shown, not lost
			errs <- err
		}()
	}
	var first error
	for range logs {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// prefixWriter writes what it is given to out a whole line at a time, each
// with prefix in front. A partial line is held back until its newline
// arrives. The mutex is shared by the writers of all the copies.
type prefixWriter struct {
	mu     *sync.Mutex
	out    io.Writer
	prefix string
	buf    []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := p.buf[:i+1]
		p.mu.Lock()
		_, err := io.WriteString(p.out, p.prefix+string(line))
		p.mu.Unlock()
		p.buf = p.buf[i+1:]
		if err != nil {
			return len(b), err
		}
	}
}

// Flush writes a held partial line, with the newline it never got.
func (p *prefixWriter) Flush() {
	if len(p.buf) == 0 {
		return
	}
	p.mu.Lock()
	io.WriteString(p.out, p.prefix+string(p.buf)+"\n")
	p.mu.Unlock()
	p.buf = nil
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

	logsCmd.Flags().IntVar(
		&logsCopy,
		"copy",
		0,
		"show only this copy's log (default: every copy)",
	)

	logsCmd.MarkFlagsOneRequired("file", "api")
	logsCmd.MarkFlagsMutuallyExclusive("file", "api")

	rootCmd.AddCommand(logsCmd)
}
