/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

var startupCmd = &cobra.Command{
	Use:   "startup",
	Short: "Print a systemd user unit that starts shinyctl-api at boot (Linux)",
	Long: `Print a systemd user unit that starts shinyctl-api at boot (Linux).

The unit goes to stdout, so it can be redirected into place. Instructions
for installing it, and any warnings, go to stderr. Nothing is installed or
started, and nothing under ~/.shinyctl is touched.`,

	RunE: func(cmd *cobra.Command, args []string) error {
		return writeStartup(os.Stdout, os.Stderr, startupEnv{
			goos:     runtime.GOOS,
			exe:      executablePath(os.Args[0], os.Executable, exec.LookPath),
			target:   resolvedPath(os.Executable),
			path:     os.Getenv("PATH"),
			tempDir:  os.TempDir(),
			lookPath: exec.LookPath,
		})
	},
}

// startupEnv is everything writeStartup reads from the machine, so tests
// can supply it.
type startupEnv struct {
	goos     string
	exe      string // how shinyctl is invoked, see executablePath
	target   string // the file exe resolves to, "" if unknown
	path     string // the PATH to give the service
	tempDir  string
	lookPath func(string) (string, error)
}

// writeStartup prints the unit to stdout and the install instructions, plus
// any warnings, to stderr.
func writeStartup(stdout, stderr io.Writer, env startupEnv) error {
	if env.goos != "linux" {
		return fmt.Errorf(`shinyctl startup only supports Linux (systemd) for now; on %s, run "shinyctl serve -d" after a reboot`, env.goos)
	}

	// Rscript's directory, so the service finds R even when the PATH of
	// the shell that ran this didn't list it (lookPath said ErrDot: it was
	// found through a relative entry).
	var rscriptDir string
	var warnings []string
	rscript, err := env.lookPath("Rscript")
	switch {
	case err == nil || errors.Is(err, exec.ErrDot):
		abs, absErr := filepath.Abs(rscript)
		if absErr != nil {
			return absErr
		}
		rscriptDir = filepath.Dir(abs)
	default:
		warnings = append(warnings, "Rscript was not found on PATH, so apps started by the service won't find it either. Add its directory to PATH and run this again.")
	}

	servicePATH, err := servicePath(env.path, rscriptDir)
	if err != nil {
		return err
	}

	unit, err := systemdUnit(env.exe, servicePATH)
	if err != nil {
		return err
	}

	if env.tempDir != "" && strings.HasPrefix(env.exe, strings.TrimRight(env.tempDir, "/")+"/") {
		warnings = append(warnings, fmt.Sprintf("%s is in a temporary directory (a go run build?), which won't survive a reboot. Install shinyctl somewhere permanent and run this again from there.", env.exe))
	}

	for _, w := range warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	if len(warnings) > 0 {
		fmt.Fprintln(stderr)
	}

	// pkill matches the daemon's command line, which is its binary path and
	// "serve". Anchoring at a path separator or the start keeps it off other
	// programs whose name merely ends in the same word. A daemon started
	// through a symlink shows the file the link points at, which can have a
	// different name, so both names are accepted.
	name := regexp.QuoteMeta(filepath.Base(env.exe))
	if env.target != "" && filepath.Base(env.target) != filepath.Base(env.exe) {
		name = "(" + name + "|" + regexp.QuoteMeta(filepath.Base(env.target)) + ")"
	}
	pattern := `(^|/)` + name + ` serve$`
	fmt.Fprintf(stderr, `To start shinyctl-api at boot, install this as a systemd user service:

  mkdir -p ~/.config/systemd/user && mkdir -p -m 700 ~/.shinyctl
  %s startup > ~/.config/systemd/user/shinyctl.service
  systemctl --user daemon-reload
  pkill -u "$USER" -f %s
  systemctl --user enable --now shinyctl
  loginctl enable-linger "$USER"

pkill stops a shinyctl-api started by hand, which would otherwise hold the
socket and make the service's copy exit. Its apps and the load balancer
keep running, and the service's shinyctl-api adopts them. enable-linger
starts your services at boot, before you log in, and keeps them running
after you log out.
`, shellQuote(env.exe), shellQuote(pattern))

	_, err = io.WriteString(stdout, unit)
	return err
}

// systemdUnit renders the unit file for the shinyctl at exe, running with
// PATH path.
func systemdUnit(exe, path string) (string, error) {
	for _, s := range []string{exe, path} {
		if i := strings.IndexFunc(s, isControl); i >= 0 {
			return "", fmt.Errorf("can't put a control character (%q) in a systemd unit", s[i])
		}
	}

	// systemd refuses to run an executable whose path has these ("Executable
	// name contains special characters"), however it is quoted.
	if i := strings.IndexAny(exe, `"'\`); i >= 0 {
		return "", fmt.Errorf("systemd can't run %s: its path contains %q", exe, exe[i])
	}

	return fmt.Sprintf(`# shinyctl-api as a systemd user service, written by "shinyctl startup".
# Paths are fixed here: run it again after moving shinyctl or R.

[Unit]
Description=shinyctl-api, which keeps R Shiny apps running

[Service]
ExecStart=%s serve
# The PATH of the shell "shinyctl startup" ran in. Apps find Rscript, and
# anything else they run, through it.
Environment=%s
# The log a shinyctl-api started by apply writes, so "shinyctl logs --api"
# shows it.
StandardOutput=append:%%h/.shinyctl/api.log
Restart=always
RestartSec=2
# Apps are child processes of shinyctl-api. Stopping or restarting the
# service stops only shinyctl-api: the apps and the load balancer keep
# serving, and the next shinyctl-api adopts them from its registry.
KillMode=process

[Install]
WantedBy=default.target
`, execWord(exe), envAssignment("PATH", path)), nil
}

// servicePath returns path for the service: without empty and relative
// entries, which a service would resolve against a directory unrelated to
// the shell's, and with rscriptDir (if any) added when it isn't listed.
func servicePath(path, rscriptDir string) (string, error) {
	var kept []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" || !filepath.IsAbs(dir) || seen[dir] {
			return
		}
		seen[dir] = true
		kept = append(kept, dir)
	}

	for _, dir := range filepath.SplitList(path) {
		add(dir)
	}
	add(rscriptDir)

	if len(kept) == 0 {
		return "", fmt.Errorf("PATH has no absolute directories to give the service")
	}
	return strings.Join(kept, ":"), nil
}

// executablePath returns the path to put in ExecStart: shinyctl as it was
// invoked, made absolute, not symlink-resolved. A user who upgrades by
// repointing ~/.local/bin/shinyctl at a new release wants the service to
// follow at its next restart, but os.Executable on Linux returns the
// resolved file, which would pin the old version. The invoked path is used
// only if it is the same file as executable() reports; otherwise
// (arg0 is not what it seems) the executable's own path is.
func executablePath(arg0 string, executable func() (string, error), lookPath func(string) (string, error)) string {
	self, err := executable()
	if err != nil {
		return arg0
	}

	invoked := arg0
	if !strings.Contains(arg0, "/") {
		found, err := lookPath(arg0)
		if err != nil && !errors.Is(err, exec.ErrDot) {
			return self
		}
		invoked = found
	}

	invoked, err = filepath.Abs(invoked)
	if err != nil {
		return self
	}

	a, errA := os.Stat(invoked)
	b, errB := os.Stat(self)
	if errA != nil || errB != nil || !os.SameFile(a, b) {
		return self
	}
	return invoked
}

// resolvedPath is the file the running shinyctl really is, with symlinks
// resolved: what a daemon started by this binary reports as its command.
// Empty if it can't be worked out.
func resolvedPath(executable func() (string, error)) string {
	self, err := executable()
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		return ""
	}
	return resolved
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// execWord quotes s, the executable, as the first word of an ExecStart
// line: % is doubled, and a word with anything but plain path characters is
// double-quoted. $ is left alone on purpose: systemd runs and checks the
// executable path as written, without the $$ to $ substitution it applies
// to the arguments, so doubling it would name a file that doesn't exist
// (systemd-analyze verify says so). Quotes and backslashes never get here;
// see systemdUnit.
func execWord(s string) string {
	escaped := strings.ReplaceAll(s, `%`, `%%`)
	if isPlain(s) {
		return escaped
	}
	return `"` + escaped + `"`
}

// envAssignment renders NAME=value as an Environment= value. It is always
// double-quoted. $ is literal there (systemd doesn't expand it in
// Environment=), but \, " and % still are special.
func envAssignment(name, value string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(value)
	return `"` + name + "=" + escaped + `"`
}

// shellQuote quotes s for a POSIX shell, only if it needs it.
func shellQuote(s string) string {
	if isPlain(s) {
		return s
	}
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

// isPlain reports whether s is non-empty and made only of characters that
// are safe unquoted in both systemd and shell words.
func isPlain(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("/._+,:@=-", r):
		default:
			return false
		}
	}
	return true
}

func init() {
	rootCmd.AddCommand(startupCmd)
}
