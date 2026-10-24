package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

func TestIsAlive(t *testing.T) {
	cases := []struct {
		name string
		spec manifest.AppSpec
		app  state.AppState
		want bool
	}{
		{
			name: "our own running process is alive, no identity check requested",
			spec: manifest.AppSpec{},
			app:  state.AppState{PID: os.Getpid(), StartedAt: time.Now()},
			want: true,
		},
		{
			name: "no OS hands out PIDs this large",
			spec: manifest.AppSpec{},
			app:  state.AppState{PID: 999999999, StartedAt: time.Now()},
			want: false,
		},
		{
			name: "zero-value AppState (never started) is not alive",
			spec: manifest.AppSpec{},
			app:  state.AppState{},
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsAlive(c.spec, c.app); got != c.want {
				t.Errorf("IsAlive(%+v, %+v) = %v, want %v", c.spec, c.app, got, c.want)
			}
		})
	}
}

// TestIsAlive_PIDReuse proves the identity check closes the PID-reuse gap:
// a genuinely-alive PID that isn't running the expected entrypoint must be
// reported as not alive — exactly what happens if a dead app's old PID
// gets recycled by the OS for something else entirely.
func TestIsAlive_PIDReuse(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found on PATH, skipping")
	}

	cmd := exec.Command(sleepPath, "2")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleep: %v", err)
	}
	defer cmd.Process.Kill()
	go cmd.Wait()

	app := state.AppState{PID: cmd.Process.Pid, StartedAt: time.Now()}

	t.Run("matching command line is alive", func(t *testing.T) {
		spec := manifest.AppSpec{EntrypointFile: "sleep"}
		if !IsAlive(spec, app) {
			t.Error("IsAlive = false, want true (command line contains \"sleep\")")
		}
	})

	t.Run("recorded entrypoint wins over the spec's", func(t *testing.T) {
		spec := manifest.AppSpec{EntrypointFile: "some-other-app.R"}
		recorded := app
		recorded.EntrypointFile = "sleep"
		if !IsAlive(spec, recorded) {
			t.Error("IsAlive = false, want true (the recorded entrypoint matches the running command)")
		}
	})

	t.Run("recorded entrypoint that does not match is not alive, even if the spec's does", func(t *testing.T) {
		spec := manifest.AppSpec{EntrypointFile: "sleep"}
		recorded := app
		recorded.EntrypointFile = "some-other-app.R"
		if IsAlive(spec, recorded) {
			t.Error("IsAlive = true, want false (the recorded entrypoint is not what is running)")
		}
	})

	t.Run("mismatched entrypoint is not alive, even though the PID is real", func(t *testing.T) {
		spec := manifest.AppSpec{EntrypointFile: "some-other-app.R"}
		if IsAlive(spec, app) {
			t.Error("IsAlive = true, want false (PID is real, but running a different command)")
		}
	})
}

func TestIsOurs(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found on PATH, skipping")
	}
	cmd := exec.Command(sleepPath, "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleep: %v", err)
	}
	defer cmd.Process.Kill()
	go cmd.Wait()

	rec := func(entrypoint string) state.AppState {
		return state.AppState{PID: cmd.Process.Pid, StartedAt: time.Now(), EntrypointFile: entrypoint}
	}

	cases := []struct {
		name      string
		app       state.AppState
		fallbacks []string
		want      bool
	}{
		{"recorded entrypoint matches", rec("sleep"), nil, true},
		{"recorded entrypoint wins over a matching fallback", rec("other.R"), []string{"sleep"}, false},
		{"no recorded entrypoint, a fallback matches", rec(""), []string{"other.R", "sleep"}, true},
		{"nothing to match is never ours", rec(""), []string{""}, false},
		{"no fallbacks at all is never ours", rec(""), nil, false},
		{"no such PID", state.AppState{PID: 999999999, StartedAt: time.Now(), EntrypointFile: "sleep"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isOurs(c.app, c.fallbacks...); got != c.want {
				t.Errorf("isOurs = %v, want %v", got, c.want)
			}
		})
	}
}

// sleepWithArg starts a process whose command line holds extra, which is how
// the tests give a stand-in the expression an R process has.
func sleepWithArg(t *testing.T, extra string) int {
	t.Helper()
	// Two commands, so the shell stays the process instead of becoming sleep.
	cmd := exec.Command("sh", "-c", "sleep 5; :", extra)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd.Process.Pid
}

// rCommandLine is the command line of an R process started with Start's
// expression: R's front-end script has turned the spaces into "~+~".
func rCommandLine(path string, port int) string {
	return strings.ReplaceAll(RunAppExpression(path, port, InternalHost), " ", "~+~")
}

// Paths with an accent, a double quote, a backslash or a space, with the
// process in a locale that has no UTF-8 at all (a minimal server, cron, a
// systemd user manager): the process is still ours.
func TestIsOurs_UnusualPathsWithoutALocale(t *testing.T) {
	t.Setenv("LC_ALL", "C")
	for _, k := range []string{"LANG", "LC_CTYPE", "LC_MESSAGES"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	for name, path := range map[string]string{
		"an accent":                  "/home/me/Données/app.R",
		"a double quote":             `/home/me/q"uote/app.R`,
		"a backslash and a space":    `/home/me/back\slash dir/app.R`,
		"everything at once":         `/home/ré "x" \y z/app.R`,
		"a space":                    "/home/me/sp ace/app.R",
		"a character outside Latin1": "/home/me/日本語/app.R",
	} {
		t.Run(name, func(t *testing.T) {
			pid := sleepWithArg(t, rCommandLine(path, 20007))
			rec := state.AppState{PID: pid, StartedAt: time.Now(), EntrypointFile: path, Port: 20007}

			if !isOurs(rec) || !IsAlive(manifest.AppSpec{}, rec) {
				t.Error("the process is not recognised")
			}
			bare := rec
			bare.EntrypointFile = ""
			if !isOurs(bare, path) {
				t.Error("the fallback does not match")
			}
			other := rec
			other.EntrypointFile = path + "x"
			if isOurs(other) {
				t.Error("another path matched")
			}
		})
	}
}

// The expected fragment comes from the text Start launches, whatever the
// path holds.
func TestPathForms_FollowTheExpression(t *testing.T) {
	for _, path := range []string{"/a/b.R", "/a b/c.R", `/a"b/c.R`, `/a\b/c.R`, "/é/ü.R", "/a\tb/c.R"} {
		line := rCommandLine(path, 20000)
		if !inCommandLine(line, path) {
			t.Errorf("%q is not found in %q", path, line)
		}
		if inCommandLine(line, path+"x") {
			t.Errorf("%qx matched %q", path, line)
		}
	}
	// R's front end run by dash (and R on macOS) collapses the doubled
	// backslash, so a path with one shows up plain.
	if !inCommandLine(`shiny::runApp("/a\b/c.R", port=1)`, `/a\b/c.R`) {
		t.Error("a backslash path is not found in its plain form")
	}
	// A plain path in an ordinary command line is found as before.
	if !inCommandLine("sleep /a b/c.R", "/a b/c.R") {
		t.Error("the plain form is not found")
	}
}

// A home directory with a space: the load balancer's process is recognised
// by its command line, which holds the path as typed.
func TestHasCommandLine_PathWithASpace(t *testing.T) {
	pid := sleepWithArg(t, "/home/a b/.shinyctl/proxy/config.json")
	if !HasCommandLine(pid, "/home/a b/.shinyctl/proxy/config.json") {
		t.Error("a plain path with a space is not found")
	}
	if HasCommandLine(pid, "/home/a b/other.json") {
		t.Error("another path matched")
	}
}

func TestProcCommandLine(t *testing.T) {
	dir := t.TempDir()
	orig := procDir
	procDir = dir
	t.Cleanup(func() { procDir = orig })

	// No /proc at all: the caller falls back to ps.
	if _, ok := procCommandLine(1); ok {
		t.Error("ok without a /proc")
	}

	os.Mkdir(filepath.Join(dir, "self"), 0o755)
	os.Mkdir(filepath.Join(dir, "42"), 0o755)
	os.WriteFile(filepath.Join(dir, "42", "cmdline"), []byte("/usr/bin/R\x00--no-echo\x00-e\x00shiny::runApp(\"/é\", port=1)\x00"), 0o644)
	os.Mkdir(filepath.Join(dir, "43"), 0o755)
	os.WriteFile(filepath.Join(dir, "43", "cmdline"), nil, 0o644) // a zombie

	line, ok := procCommandLine(42)
	if !ok || line != `/usr/bin/R --no-echo -e shiny::runApp("/é", port=1)` {
		t.Errorf("procCommandLine(42) = %q, %v", line, ok)
	}
	if line, ok := procCommandLine(99); !ok || line != "" {
		t.Errorf("a process that is not there: %q, %v", line, ok)
	}
	if _, ok := commandLine(43); ok {
		t.Error("an empty command line (a zombie) counts as a command line")
	}
}

// fakeProc makes a /proc with one process: its cmdline and stat.
func fakeProc(t *testing.T, pid int, cmdline, stat string) (cmdlinePath string) {
	t.Helper()
	dir := t.TempDir()
	orig := procDir
	procDir = dir
	t.Cleanup(func() { procDir = orig })
	os.Mkdir(filepath.Join(dir, "self"), 0o755)
	pdir := filepath.Join(dir, strconv.Itoa(pid))
	os.Mkdir(pdir, 0o755)
	cmdlinePath = filepath.Join(pdir, "cmdline")
	os.WriteFile(cmdlinePath, []byte(cmdline), 0o644)
	if stat != "" {
		os.WriteFile(filepath.Join(pdir, "stat"), []byte(stat), 0o644)
	}
	return cmdlinePath
}

func shortProcWait(t *testing.T, wait time.Duration) {
	t.Helper()
	orig := procWait
	procWait = wait
	t.Cleanup(func() { procWait = orig })
}

// Right after a start the file is empty for a moment; a process that is alive
// is waited for.
func TestProcCommandLine_EmptyThenFilled(t *testing.T) {
	path := fakeProc(t, 42, "", "42 (R) S 1 42 42 0 -1")
	shortProcWait(t, 2*time.Second)
	go func() {
		time.Sleep(20 * time.Millisecond)
		os.WriteFile(path, []byte("/usr/bin/R\x00-e\x00x\x00"), 0o644)
	}()
	start := time.Now()
	line, ok := procCommandLine(42)
	if !ok || line != "/usr/bin/R -e x" {
		t.Errorf("procCommandLine = %q, %v; want the line once it is there", line, ok)
	}
	if took := time.Since(start); took < 15*time.Millisecond || took > time.Second {
		t.Errorf("took %v, want about the 20 ms the file took to fill", took)
	}
}

func TestProcCommandLine_ZombieOrMissingIsGoneAtOnce(t *testing.T) {
	shortProcWait(t, 5*time.Second)
	for name, stat := range map[string]string{
		"zombie":  "42 (R) Z 1 42 42 0 -1",
		"dead":    "42 (R) X 1 42 42 0 -1",
		"no stat": "",
	} {
		fakeProc(t, 42, "", stat)
		start := time.Now()
		if line, ok := procCommandLine(42); !ok || line != "" {
			t.Errorf("%s: %q, %v", name, line, ok)
		}
		if took := time.Since(start); took > 500*time.Millisecond {
			t.Errorf("%s: waited %v for a process that is gone", name, took)
		}
	}
}

func TestProcCommandLine_EmptyForGoodIsNotOursAfterTheDeadline(t *testing.T) {
	fakeProc(t, 42, "", "42 (R) S 1 42 42 0 -1")
	shortProcWait(t, 30*time.Millisecond)
	start := time.Now()
	line, ok := procCommandLine(42)
	took := time.Since(start)
	if !ok || line != "" {
		t.Errorf("procCommandLine = %q, %v", line, ok)
	}
	if took < 25*time.Millisecond || took > time.Second {
		t.Errorf("waited %v, want the 30 ms deadline", took)
	}
	if _, found := commandLine(42); found {
		t.Error("an empty command line counts as one")
	}
}

// A kernel thread (PF_KTHREAD) has an empty command line for good: it is not
// waited for. A normal process with an empty one still is.
func TestProcCommandLine_KernelThreadIsNotWaitedFor(t *testing.T) {
	shortProcWait(t, 500*time.Millisecond)

	fakeProc(t, 2, "", "2 (kthreadd) S 0 0 0 0 -1 2129984 0 0 0 0 0 0")
	start := time.Now()
	if line, ok := procCommandLine(2); !ok || line != "" {
		t.Errorf("procCommandLine = %q, %v", line, ok)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("waited %v for a kernel thread", took)
	}
	if _, found := commandLine(2); found {
		t.Error("a kernel thread has a command line")
	}

	// The same flags field without PF_KTHREAD: a process being set up.
	fakeProc(t, 3, "", "3 (sh) S 1 3 3 0 -1 4194560 0 0 0 0 0 0")
	start = time.Now()
	procCommandLine(3)
	if took := time.Since(start); took < 450*time.Millisecond {
		t.Errorf("a normal process with an empty command line was given up after %v", took)
	}

	// And a name that looks like flags doesn't confuse the fields.
	fakeProc(t, 4, "", "4 (a) S (2097152 1 4 4 0 -1 4194560 0 0")
	start = time.Now()
	procCommandLine(4)
	if took := time.Since(start); took < 450*time.Millisecond {
		t.Errorf("the name was read as the flags: gave up after %v", took)
	}
}

// The name of a process can hold spaces and parentheses; the state is what
// follows the last ")".
func TestProcCommandLine_StateAfterTheLastParenthesis(t *testing.T) {
	shortProcWait(t, 2*time.Second)
	// Read from the first ")" this would say Z and give up at once.
	path := fakeProc(t, 42, "", "42 (a) Z (b) R 1 42 42 0 -1")
	go func() {
		time.Sleep(20 * time.Millisecond)
		os.WriteFile(path, []byte("x\x00y\x00"), 0o644)
	}()
	if line, _ := procCommandLine(42); line != "x y" {
		t.Errorf("line = %q: the name was taken for the state", line)
	}

	// And a zombie whose name looks alive.
	fakeProc(t, 43, "", "43 (a) R (b) Z 1 43 43 0 -1")
	start := time.Now()
	if line, _ := procCommandLine(43); line != "" || time.Since(start) > 500*time.Millisecond {
		t.Errorf("a zombie was waited for: %q after %v", line, time.Since(start))
	}
}

// Identity checks made at once after a Start, as the daemon's own functions
// make them: on Linux the command line is empty for a moment after the exec.
// 300 starts take about 3 s; without the retry this fails in a container with
// one CPU (docker run --cpus=1) in every run.
func TestIdentity_RightAfterStart(t *testing.T) {
	const n = 300
	spec := manifest.AppSpec{EntrypointFile: "/srv/app.R"}
	arg := rCommandLine("/srv/app.R", 20001)
	var notOurs int
	for i := 0; i < n; i++ {
		cmd := exec.Command("sh", "-c", "sleep 5; :", "x", arg)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		rec := state.AppState{PID: cmd.Process.Pid, StartedAt: time.Now(), EntrypointFile: "/srv/app.R", Port: 20001, Copy: 1}
		if !isOurs(rec) || !IsAlive(spec, rec) {
			notOurs++
		}
		cmd.Process.Kill()
		cmd.Wait()
	}
	if notOurs > 0 {
		t.Errorf("of %d starts: isOurs/IsAlive wrong %d", n, notOurs)
	}
}
