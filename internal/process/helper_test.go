package process

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The test binary doubles as the processes a copy is made of. A shell is no
// good for this on macOS: /bin/sh replaces its own image right after the start
// and ps can read its command line in the middle of that (see sleeperEnv).
// helperEnv names what the binary is to be:
//
//	worker   sleeps; helperFlags lists what it does first: setsid (a session
//	         and group of its own, as processx does), ignore (shrugs off
//	         SIGTERM), then it tells its parent it is ready through descriptor 3
//	leader   starts the children named by helperChildren, waits until each is
//	         ready, writes "kind pid" lines to helperOut, and sleeps
//	middle   starts a setsid worker, prints its pid and exits at once, so the
//	         worker is no longer anyone's descendant
//	daemon   stands in for an older shinyctl-api: starts a leader in the
//	         daemon's own group and sleeps
//
// Every helper's command line holds "sleep", the entrypoint the tests record.
const (
	helperEnv      = "SHINYCTL_TEST_HELPER"
	helperFlags    = "SHINYCTL_TEST_FLAGS"
	helperChildren = "SHINYCTL_TEST_CHILDREN"
	helperOut      = "SHINYCTL_TEST_OUT"
)

func runHelper(mode string) {
	flags := os.Getenv(helperFlags)
	switch mode {
	case "worker":
		if strings.Contains(flags, "setsid") {
			syscall.Setsid()
		}
		if strings.Contains(flags, "ignore") {
			signal.Ignore(syscall.SIGTERM)
		}
		if f := os.NewFile(3, "ready"); f != nil {
			f.Write([]byte("x"))
			f.Close()
		}
		time.Sleep(40 * time.Second)
	case "leader":
		if strings.Contains(flags, "ignore") {
			signal.Ignore(syscall.SIGTERM)
		}
		var lines []string
		for _, kind := range strings.Split(os.Getenv(helperChildren), ",") {
			if kind == "" {
				continue
			}
			pid, err := startHelperChild(kind)
			if err != nil {
				fmt.Fprintln(os.Stderr, "leader:", err)
				os.Exit(2)
			}
			lines = append(lines, fmt.Sprintf("%s %d", kind, pid))
		}
		out := os.Getenv(helperOut)
		os.WriteFile(out+".tmp", []byte(strings.Join(lines, "\n")+"\n"), 0o644)
		os.Rename(out+".tmp", out)
		time.Sleep(40 * time.Second)
	case "middle":
		pid, err := startHelperChild("setsid")
		if err != nil {
			os.Exit(2)
		}
		fmt.Println(pid)
	case "daemon":
		exe, _ := os.Executable()
		cmd := exec.Command(exe, "sleep", "leader")
		cmd.Env = append(os.Environ(), helperEnv+"=leader")
		cmd.Start()
		go cmd.Wait()
		time.Sleep(40 * time.Second)
	}
	os.Exit(0)
}

// startHelperChild starts the child of kind (plain, ignore, setsid,
// setsid+ignore, orphan) and returns its pid once it is ready. An orphan is a
// setsid worker whose parent has already exited.
func startHelperChild(kind string) (int, error) {
	exe, _ := os.Executable()
	r, w, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer r.Close()

	mode, fl := "worker", ""
	switch kind {
	case "ignore":
		fl = "ignore"
	case "setsid":
		fl = "setsid"
	case "setsid+ignore":
		fl = "setsid ignore"
	case "orphan":
		mode = "middle"
	}
	cmd := exec.Command(exe, "sleep", mode)
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, helperFlags+"="+fl)
	cmd.ExtraFiles = []*os.File{w}
	if mode == "middle" {
		out, err := cmd.Output()
		w.Close()
		if err != nil {
			return 0, err
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return 0, err
		}
		// The worker has said nothing on this pipe: wait until it is ready by
		// the process being its own session leader.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if sid, err := unix.Getsid(pid); err == nil && sid == pid {
				return pid, nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return 0, fmt.Errorf("the orphan %d never became a session leader", pid)
	}
	if err := cmd.Start(); err != nil {
		w.Close()
		return 0, err
	}
	w.Close()
	go cmd.Wait()
	if _, err := bufio.NewReader(r).ReadByte(); err != nil {
		return 0, fmt.Errorf("%s child %d never got ready: %w", kind, cmd.Process.Pid, err)
	}
	return cmd.Process.Pid, nil
}

// tree is a copy made of helper processes, as the tests start it.
type tree struct {
	leader   *exec.Cmd
	children map[string][]int // pids by kind
}

// helperCommand is a helper of mode started from this test binary, with the
// command line the tests record as the entrypoint.
func helperCommand(mode string, env ...string) *exec.Cmd {
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "sleep", mode)
	cmd.Env = append(os.Environ(), append([]string{helperEnv + "=" + mode}, env...)...)
	return cmd
}

// startTree starts a leader of its own group (ownGroup) or in the test's own
// group, with children of the kinds listed, and returns it once they are all
// ready. Everything is ended when the test is: the leader's group and each
// child by pid, since a session of its own is not in the group.
func startTree(t *testing.T, ownGroup bool, leaderFlags string, kinds ...string) *tree {
	t.Helper()
	out := filepath.Join(t.TempDir(), "pids")
	cmd := helperCommand("leader", helperFlags+"="+leaderFlags, helperChildren+"="+strings.Join(kinds, ","), helperOut+"="+out)
	if ownGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	tr := &tree{leader: cmd, children: map[string][]int{}}
	t.Cleanup(func() {
		if ownGroup {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		cmd.Process.Kill()
		for _, pids := range tr.children {
			for _, pid := range pids {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	tr.read(t, out)
	return tr
}

func (tr *tree) read(t *testing.T, out string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(out); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				kind, pidText, ok := strings.Cut(line, " ")
				if !ok {
					continue
				}
				pid, _ := strconv.Atoi(pidText)
				tr.children[kind] = append(tr.children[kind], pid)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the leader never reported its children")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (tr *tree) pid() int { return tr.leader.Process.Pid }

// ours is the identity check of the leader's record.
func (tr *tree) ours() func() bool {
	app := sleeperApp(tr.pid(), "sleep")
	return func() bool { return isOurs(app) }
}

// gone waits briefly for pid to be gone: a process whose parent has just
// died is a zombie until init reaps it.
func gone(pid int) bool { return waitGone(pid, 3*time.Second) }

// running says pid is alive and not a zombie.
func running(t *testing.T, pid int) bool {
	t.Helper()
	table, err := readTable()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range table {
		if p.pid == pid {
			return !p.zombie
		}
	}
	return false
}
