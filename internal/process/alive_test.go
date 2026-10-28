package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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

// sleeperEnv makes the test binary a sleeper instead of running the tests:
// a process that is started once, with a command line that never changes.
// (A shell that starts sleep, or /bin/sh on macOS, which is a launcher for
// bash, replaces its own image right after the start, and ps can read the
// command line in the middle of that.)
const sleeperEnv = "SHINYCTL_TEST_SLEEPER"

func TestMain(m *testing.M) {
	if os.Getenv(sleeperEnv) == "1" {
		time.Sleep(30 * time.Second)
		return
	}
	os.Exit(m.Run())
}

// startArgSleeper starts a sleeper whose command line is "sleep" and then args.
func startArgSleeper(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, append([]string{"sleep"}, args...)...)
	cmd.Env = append(os.Environ(), sleeperEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return cmd
}

// sleepWithArg starts a process whose command line holds extra, which is
// how the tests give a stand-in the `port=<port>,` an R process has.
func sleepWithArg(t *testing.T, extra string) int {
	t.Helper()
	return startArgSleeper(t, extra).Process.Pid
}

func TestIsOurs_PortMustMatch(t *testing.T) {
	pid := sleepWithArg(t, "port=20001,")

	rec := func(port int) state.AppState {
		return state.AppState{PID: pid, StartedAt: time.Now(), EntrypointFile: "sleep", Port: port}
	}
	cases := []struct {
		name string
		app  state.AppState
		want bool
	}{
		{"the port is in the command line", rec(20001), true},
		{"another copy's port is not ours", rec(20002), false},
		{"a prefix of the port is not ours", rec(2000), false},
		{"a record without a port is matched on the entrypoint alone", rec(0), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isOurs(c.app); got != c.want {
				t.Errorf("isOurs = %v, want %v", got, c.want)
			}
			if got := IsAlive(manifest.AppSpec{}, c.app); got != c.want {
				t.Errorf("IsAlive = %v, want %v", got, c.want)
			}
		})
	}

	// Also for a record that has no entrypoint of its own.
	noEntry := rec(20002)
	noEntry.EntrypointFile = ""
	if isOurs(noEntry, "sleep") {
		t.Error("a fallback must not get past a port that differs")
	}
}

func TestAliveAll(t *testing.T) {
	one := sleepWithArg(t, "port=20001,")
	two := sleepWithArg(t, "port=20002,")

	started := time.Now().Add(-time.Minute) // not fresh: see freshWindow
	spec := manifest.AppSpec{EntrypointFile: "sleep"}
	recs := []state.AppState{
		{PID: one, StartedAt: started, EntrypointFile: "sleep", Port: 20001, Copy: 1},
		{PID: two, StartedAt: started, EntrypointFile: "sleep", Port: 20002, Copy: 2},
		{PID: one, StartedAt: started, EntrypointFile: "sleep", Port: 20002, Copy: 3}, // another copy's pid
		{PID: 999999999, StartedAt: started, EntrypointFile: "sleep", Port: 20004, Copy: 4},
		{Copy: 5}, // never started
		{PID: two, StartedAt: started, Port: 20002, Copy: 6},                            // no recorded entrypoint: the spec's
		{PID: two, StartedAt: started, EntrypointFile: "other.R", Port: 20002, Copy: 7}, // not this app
	}
	want := []bool{true, true, false, false, false, true, false}

	var calls int
	orig := psRun
	t.Cleanup(func() { psRun = orig })
	psRun = func(args ...string) ([]byte, error) {
		calls++
		return orig(args...)
	}

	got := AliveAll(spec, recs)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AliveAll = %v, want %v", got, want)
	}
	if calls > 1 {
		t.Errorf("ps was run %d times, want at most once (not at all where /proc is read)", calls)
	}
	for i, rec := range recs {
		if single := IsAlive(spec, rec); single != got[i] {
			t.Errorf("record %d: IsAlive = %v but AliveAll = %v", i, single, got[i])
		}
	}

	calls = 0
	if got := AliveAll(spec, []state.AppState{{Copy: 1}, {PID: 999999999, StartedAt: time.Now()}}); calls != 0 || got[0] || got[1] {
		t.Errorf("nothing running: %v after %d ps calls, want none and no ps", got, calls)
	}
	if got := AliveAll(spec, nil); len(got) != 0 {
		t.Errorf("AliveAll(nil) = %v", got)
	}
}

func TestCommandLines(t *testing.T) {
	orig := psRun
	t.Cleanup(func() { psRun = orig })
	var args []string
	psRun = func(a ...string) ([]byte, error) {
		args = a
		return []byte("  12 /usr/bin/R --no-echo -e shiny::runApp(\"/a b.R\", port=20000,~+~host=\"127.0.0.1\")\n13 sleep 5\n\nnot a line\n"), nil
	}

	got := commandLinesPS([]int{12, 13, 14})
	if len(got) != 2 || !strings.Contains(got[12], "port=20000,") || got[13] != "sleep 5" {
		t.Errorf("commandLines = %v", got)
	}
	if strings.Join(args, " ") != "-o pid=,command= -p 12,13,14" {
		t.Errorf("ps args = %v", args)
	}
}

// R's front-end script turns the spaces of a -e argument into "~+~", so the
// process of an app in a folder with a space shows the path that way.
func TestIsOurs_PathWithASpace(t *testing.T) {
	pid := sleepWithArg(t, `shiny::runApp("/x/sp~+~ace/app.R",~+~port=20003,~+~host="127.0.0.1")`)
	spec := manifest.AppSpec{EntrypointFile: "/x/sp ace/app.R"}
	rec := state.AppState{PID: pid, StartedAt: time.Now(), EntrypointFile: "/x/sp ace/app.R", Port: 20003}

	if !isOurs(rec) || !IsAlive(spec, rec) {
		t.Error("a copy whose path has a space is not recognised")
	}
	if got := AliveAll(spec, []state.AppState{rec}); !got[0] {
		t.Error("AliveAll does not recognise it either")
	}

	// A record without an entrypoint of its own is matched by the fallback.
	bare := rec
	bare.EntrypointFile = ""
	if !isOurs(bare, "/x/sp ace/app.R") {
		t.Error("the fallback with a space does not match")
	}

	// Another folder, or another port, is still not ours.
	other := rec
	other.EntrypointFile = "/x/sp ace/other.R"
	if isOurs(other) {
		t.Error("another entrypoint matched")
	}
	other = rec
	other.Port = 20004
	if isOurs(other) {
		t.Error("another port matched")
	}
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
			spec := manifest.AppSpec{EntrypointFile: path}
			rec := state.AppState{PID: pid, StartedAt: time.Now(), EntrypointFile: path, Port: 20007}

			if !isOurs(rec) || !IsAlive(spec, rec) {
				t.Error("the process is not recognised")
			}
			if got := AliveAll(spec, []state.AppState{rec}); !got[0] {
				t.Error("AliveAll does not recognise it")
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
	if got := commandLines([]int{42, 43, 99}); len(got) != 1 || got[42] != line42(t) {
		t.Errorf("commandLines = %v", got)
	}
	if _, ok := commandLine(43); ok {
		t.Error("an empty command line (a zombie) counts as a command line")
	}
}

func line42(t *testing.T) string {
	l, _ := procCommandLine(42)
	return l
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

// The batch waits once for all of them, not once for each.
func TestCommandLines_OneDeadlineForTheBatch(t *testing.T) {
	dir := t.TempDir()
	orig, origWait := procDir, procWait
	procDir, procWait = dir, 100*time.Millisecond
	t.Cleanup(func() { procDir, procWait = orig, origWait })
	os.Mkdir(filepath.Join(dir, "self"), 0o755)
	for _, pid := range []int{10, 11, 12, 13, 14} {
		d := filepath.Join(dir, strconv.Itoa(pid))
		os.Mkdir(d, 0o755)
		os.WriteFile(filepath.Join(d, "cmdline"), nil, 0o644)
		os.WriteFile(filepath.Join(d, "stat"), []byte(strconv.Itoa(pid)+" (R) S 1"), 0o644)
	}
	// 10 fills after a while; 11 is a zombie; the rest stay empty.
	os.WriteFile(filepath.Join(dir, "11", "stat"), []byte("11 (R) Z 1"), 0o644)
	go func() {
		time.Sleep(30 * time.Millisecond)
		os.WriteFile(filepath.Join(dir, "10", "cmdline"), []byte("a\x00b\x00"), 0o644)
	}()

	start := time.Now()
	got := commandLines([]int{10, 11, 12, 13, 14, 99})
	took := time.Since(start)
	if len(got) != 1 || got[10] != "a b" {
		t.Errorf("commandLines = %v, want only 10", got)
	}
	if took < 90*time.Millisecond || took > 450*time.Millisecond {
		t.Errorf("took %v for three empty ones: want about one deadline (100 ms), not one each", took)
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
	var notOurs, aliveAllNo, batchMissing int
	for i := 0; i < n; i++ {
		cmd := startArgSleeper(t, "x", arg)
		rec := state.AppState{PID: cmd.Process.Pid, StartedAt: time.Now(), EntrypointFile: "/srv/app.R", Port: 20001, Copy: 1}
		if !isOurs(rec) || !IsAlive(spec, rec) {
			notOurs++
		}
		if !AliveAll(spec, []state.AppState{rec})[0] {
			aliveAllNo++
		}
		if _, ok := commandLines([]int{rec.PID})[rec.PID]; !ok {
			batchMissing++
		}
		cmd.Process.Kill()
		cmd.Wait()
	}
	if notOurs+aliveAllNo+batchMissing > 0 {
		t.Errorf("of %d starts: isOurs/IsAlive wrong %d, AliveAll wrong %d, batch missing %d", n, notOurs, aliveAllNo, batchMissing)
	}
}

// A process that has only just started can be read by ps in the middle of
// replacing its image: what it prints is not yet its command line. A fresh
// record is read again a few times before it is called someone else's; an old
// one is not.
func TestIdentity_AFreshRecordIsReadAgainWhenPsCatchesItMidExec(t *testing.T) {
	origProc, origPs := procDir, psRun
	t.Cleanup(func() { procDir, psRun = origProc, origPs })
	procDir = t.TempDir() + "/none" // command lines come from ps here, as on macOS

	pid := sleepWithArg(t, "port=20001,")
	rec := state.AppState{PID: pid, StartedAt: time.Now(), EntrypointFile: "sleep", Port: 20001, Copy: 1}
	spec := manifest.AppSpec{EntrypointFile: "sleep"}

	calls := 0
	psRun = func(args ...string) ([]byte, error) {
		calls++
		if calls <= 2 {
			return []byte("/some/other/image --still-starting\n"), nil // caught mid-exec
		}
		return origPs(args...)
	}
	if !isOurs(rec, "sleep") {
		t.Error("isOurs: a fresh record caught mid-exec was called someone else's")
	}
	calls = 0
	if got := AliveAll(spec, []state.AppState{rec}); !got[0] {
		t.Errorf("AliveAll: a fresh record caught mid-exec was called dead (%d ps calls)", calls)
	}

	// The same misreading of an old record, or of a PID that is a stranger's,
	// is answered at once, with one look.
	old := rec
	old.StartedAt = time.Now().Add(-time.Minute)
	calls = 0
	psRun = func(args ...string) ([]byte, error) { calls++; return []byte("/some/other/image\n"), nil }
	if isOurs(old, "sleep") || calls != 1 {
		t.Errorf("an old record: isOurs answered with %d ps calls, want false after one", calls)
	}
	calls = 0
	if isOurs(rec, "sleep") || calls != freshTries+1 {
		t.Errorf("a fresh record that never matches: %d ps calls, want %d and false", calls, freshTries+1)
	}
}
