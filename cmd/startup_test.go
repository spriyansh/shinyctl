/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const goldenExe = "/home/me/go/bin/shinyctl"

func rscriptAt(path string) func(string) (string, error) {
	return func(string) (string, error) { return path, nil }
}

func linuxEnv(exe, path string) startupEnv {
	return startupEnv{goos: "linux", exe: exe, path: path, tempDir: "/tmp", lookPath: rscriptAt("/usr/bin/Rscript")}
}

func runStartup(t *testing.T, env startupEnv) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = writeStartup(&out, &errOut, env)
	return out.String(), errOut.String(), err
}

func TestStartup_Golden(t *testing.T) {
	stdout, stderr, err := runStartup(t, linuxEnv(goldenExe, "/home/me/bin:/usr/local/bin:/usr/bin:/bin"))
	if err != nil {
		t.Fatal(err)
	}

	wantUnit := `# shinyctl-api as a systemd user service, written by "shinyctl startup".
# Paths are fixed here: run it again after moving shinyctl or R.

[Unit]
Description=shinyctl-api, which keeps R Shiny apps running

[Service]
ExecStart=/home/me/go/bin/shinyctl serve
# The PATH of the shell "shinyctl startup" ran in. Apps find Rscript, and
# anything else they run, through it.
Environment="PATH=/home/me/bin:/usr/local/bin:/usr/bin:/bin"
# The log a shinyctl-api started by apply writes, so "shinyctl logs --api"
# shows it.
StandardOutput=append:%h/.shinyctl/api.log
Restart=always
RestartSec=2
# Apps are child processes of shinyctl-api. Stopping or restarting the
# service stops only shinyctl-api: the apps keep serving, and the next
# shinyctl-api adopts them from its registry.
KillMode=process

[Install]
WantedBy=default.target
`
	if stdout != wantUnit {
		t.Errorf("stdout =\n%s\nwant\n%s", stdout, wantUnit)
	}

	wantInstructions := `To start shinyctl-api at boot, install this as a systemd user service:

  mkdir -p ~/.config/systemd/user && mkdir -p -m 700 ~/.shinyctl
  /home/me/go/bin/shinyctl startup > ~/.config/systemd/user/shinyctl.service
  systemctl --user daemon-reload
  pkill -u "$USER" -f '(^|/)shinyctl serve$'
  systemctl --user enable --now shinyctl
  loginctl enable-linger "$USER"

pkill stops a shinyctl-api started by hand, which would otherwise hold the
socket and make the service's copy exit. Its apps keep running, and the
service's shinyctl-api adopts them. enable-linger starts your services at
boot, before you log in, and keeps them running after you log out.
`
	if stderr != wantInstructions {
		t.Errorf("stderr =\n%s\nwant\n%s", stderr, wantInstructions)
	}
}

func TestStartup_Quoting(t *testing.T) {
	t.Run("exe path", func(t *testing.T) {
		exe := `/opt/my apps/50%/x$y/shinyctl`
		stdout, stderr, err := runStartup(t, linuxEnv(exe, "/usr/bin"))
		if err != nil {
			t.Fatal(err)
		}

		if want := `ExecStart="/opt/my apps/50%%/x$y/shinyctl" serve` + "\n"; !strings.Contains(stdout, want) {
			t.Errorf("unit does not contain %q:\n%s", want, stdout)
		}
		if want := `  '/opt/my apps/50%/x$y/shinyctl' startup > `; !strings.Contains(stderr, want) {
			t.Errorf("instructions do not contain %q:\n%s", want, stderr)
		}
	})

	t.Run("PATH", func(t *testing.T) {
		stdout, _, err := runStartup(t, linuxEnv(goldenExe, `/opt/my bin:/opt/50%:/opt/q"uote:/opt/back\slash:/opt/$HOME`))
		if err != nil {
			t.Fatal(err)
		}

		want := `Environment="PATH=/opt/my bin:/opt/50%%:/opt/q\"uote:/opt/back\\slash:/opt/$HOME:/usr/bin"` + "\n"
		if !strings.Contains(stdout, want) {
			t.Errorf("unit does not contain %q:\n%s", want, stdout)
		}
	})

	t.Run("empty and relative PATH entries are dropped", func(t *testing.T) {
		stdout, _, err := runStartup(t, linuxEnv(goldenExe, ":/usr/bin::.:bin"))
		if err != nil {
			t.Fatal(err)
		}

		if want := `Environment="PATH=/usr/bin"` + "\n"; !strings.Contains(stdout, want) {
			t.Errorf("unit does not contain %q:\n%s", want, stdout)
		}
	})

	t.Run("a PATH with nothing absolute is an error", func(t *testing.T) {
		env := linuxEnv(goldenExe, ":.:bin")
		env.lookPath = func(string) (string, error) { return "", errors.New("not found") }
		stdout, _, err := runStartup(t, env)
		if err == nil || stdout != "" {
			t.Errorf("got error %v and stdout %q, want an error and no output", err, stdout)
		}
	})
}

func TestStartup_RscriptDirAdded(t *testing.T) {
	env := linuxEnv(goldenExe, "/usr/bin")
	env.lookPath = rscriptAt("/opt/R/bin/Rscript")

	stdout, _, err := runStartup(t, env)
	if err != nil {
		t.Fatal(err)
	}
	if want := `Environment="PATH=/usr/bin:/opt/R/bin"` + "\n"; !strings.Contains(stdout, want) {
		t.Errorf("unit does not contain %q:\n%s", want, stdout)
	}
}

func TestStartup_RscriptFoundThroughRelativeEntry(t *testing.T) {
	env := linuxEnv(goldenExe, "/usr/bin:rbin")
	env.lookPath = func(string) (string, error) { return "rbin/Rscript", exec.ErrDot }

	stdout, _, err := runStartup(t, env)
	if err != nil {
		t.Fatal(err)
	}

	abs, err := filepath.Abs("rbin")
	if err != nil {
		t.Fatal(err)
	}
	if want := `Environment="PATH=/usr/bin:` + abs + `"` + "\n"; !strings.Contains(stdout, want) {
		t.Errorf("unit does not contain %q:\n%s", want, stdout)
	}
}

func TestStartup_NoRscript(t *testing.T) {
	env := linuxEnv(goldenExe, "/usr/bin")
	env.lookPath = func(string) (string, error) { return "", errors.New("not found") }

	stdout, stderr, err := runStartup(t, env)
	if err != nil {
		t.Fatalf("error = %v, want the unit printed anyway", err)
	}
	if !strings.Contains(stdout, "ExecStart=") {
		t.Errorf("stdout has no unit:\n%s", stdout)
	}
	if !strings.HasPrefix(stderr, "warning: Rscript was not found on PATH") {
		t.Errorf("stderr does not start with the Rscript warning:\n%s", stderr)
	}
}

func TestStartup_NotLinux(t *testing.T) {
	env := linuxEnv(goldenExe, "/usr/bin")
	env.goos = "darwin"

	stdout, _, err := runStartup(t, env)
	want := `shinyctl startup only supports Linux (systemd) for now; on darwin, run "shinyctl serve -d" after a reboot`
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
}

func TestStartup_ControlCharacters(t *testing.T) {
	for name, env := range map[string]startupEnv{
		"newline in PATH": linuxEnv(goldenExe, "/usr/bin:/opt/a\nExecStartPre=/bin/evil"),
		"newline in exe":  linuxEnv("/opt/a\nb/shinyctl", "/usr/bin"),
		"tab in exe":      linuxEnv("/opt/a\tb/shinyctl", "/usr/bin"),
		"quote in exe":    linuxEnv(`/opt/a"b/shinyctl`, "/usr/bin"),
		"apostrophe":      linuxEnv(`/opt/a'b/shinyctl`, "/usr/bin"),
		"backslash":       linuxEnv(`/opt/a\b/shinyctl`, "/usr/bin"),
	} {
		t.Run(name, func(t *testing.T) {
			stdout, _, err := runStartup(t, env)
			if err == nil || stdout != "" {
				t.Errorf("got error %v and stdout %q, want an error and no output", err, stdout)
			}
		})
	}
}

func TestStartup_TemporaryBinaryWarns(t *testing.T) {
	env := linuxEnv("/tmp/go-build123/b001/exe/shinyctl", "/usr/bin")

	_, stderr, err := runStartup(t, env)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "warning: /tmp/go-build123/b001/exe/shinyctl is in a temporary directory") {
		t.Errorf("stderr has no temporary-directory warning:\n%s", stderr)
	}

	env.exe = goldenExe
	if _, stderr, _ := runStartup(t, env); strings.Contains(stderr, "warning") {
		t.Errorf("stderr warns about a permanent install:\n%s", stderr)
	}
}

func TestExecutablePath(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "releases", "shinyctl_0.2.0", "shinyctl")
	if err := os.MkdirAll(filepath.Dir(real), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("x"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bin", "shinyctl")
	if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("y"), 0755); err != nil {
		t.Fatal(err)
	}

	// os.Executable on Linux reports the resolved file.
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	executable := func() (string, error) { return resolved, nil }

	t.Run("a symlink path is kept unresolved", func(t *testing.T) {
		if got := executablePath(link, executable, exec.LookPath); got != link {
			t.Errorf("executablePath() = %q, want %q", got, link)
		}
	})

	t.Run("a bare name is found through PATH", func(t *testing.T) {
		t.Setenv("PATH", filepath.Dir(link))
		if got := executablePath("shinyctl", executable, exec.LookPath); got != link {
			t.Errorf("executablePath() = %q, want %q", got, link)
		}
	})

	t.Run("a path naming a different file falls back to the executable", func(t *testing.T) {
		if got := executablePath(other, executable, exec.LookPath); got != resolved {
			t.Errorf("executablePath() = %q, want %q", got, resolved)
		}
	})
}

// The pkill pattern has to match the command line of a shinyctl-api that
// LaunchDetached started (the binary's path, then "serve") and nothing
// else of ours.
func TestStartup_PkillPatternMatchesDaemonCommandLine(t *testing.T) {
	_, stderr, err := runStartup(t, linuxEnv(goldenExe, "/usr/bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, `-f '(^|/)shinyctl serve$'`) {
		t.Fatalf("instructions have no pkill pattern:\n%s", stderr)
	}
	pattern := regexp.MustCompile(`(^|/)shinyctl serve$`)

	for _, cmdline := range []string{
		"/home/me/go/bin/shinyctl serve",
		"/usr/local/bin/shinyctl serve",
		"shinyctl serve",
	} {
		if !pattern.MatchString(cmdline) {
			t.Errorf("pattern does not match the daemon %q", cmdline)
		}
	}
	for _, cmdline := range []string{
		"/home/me/go/bin/shinyctl logs -f x.yaml --follow",
		"/home/me/go/bin/shinyctl status",
		"/home/me/go/bin/shinyctl serve -d",
		"/home/me/bin/notshinyctl serve",
		"vim shinyctl serve.txt",
		"/home/me/go/bin/shinyctl proxy --config /home/me/.shinyctl/proxy/config.json",
	} {
		if pattern.MatchString(cmdline) {
			t.Errorf("pattern matches %q", cmdline)
		}
	}
}

func TestStartup_PkillPatternEscapesTheBinaryName(t *testing.T) {
	_, stderr, err := runStartup(t, linuxEnv("/usr/local/bin/shinyctl.v2", "/usr/bin"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `-f '(^|/)shinyctl\.v2 serve$'`; !strings.Contains(stderr, want) {
		t.Errorf("instructions do not contain %s:\n%s", want, stderr)
	}
}

// A daemon started through a symlink reports the file the link points at,
// so the pkill pattern accepts that name too.
func TestStartup_PkillPatternAcceptsSymlinkTarget(t *testing.T) {
	env := linuxEnv("/usr/local/bin/shinyctl", "/usr/bin")
	env.target = "/opt/releases/shinyctl-linux-amd64"

	_, stderr, err := runStartup(t, env)
	if err != nil {
		t.Fatal(err)
	}

	const want = `-f '(^|/)(shinyctl|shinyctl-linux-amd64) serve$'`
	if !strings.Contains(stderr, want) {
		t.Fatalf("instructions do not contain %s:\n%s", want, stderr)
	}
	pattern := regexp.MustCompile(`(^|/)(shinyctl|shinyctl-linux-amd64) serve$`)
	for _, cmdline := range []string{"/usr/local/bin/shinyctl serve", "/opt/releases/shinyctl-linux-amd64 serve"} {
		if !pattern.MatchString(cmdline) {
			t.Errorf("pattern does not match %q", cmdline)
		}
	}
}
