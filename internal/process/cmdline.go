package process

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// RunAppExpression is the R expression that runs an app: the one command line
// Start launches, so that what identifies a process afterwards (see
// inCommandLine) is derived from the same text.
func RunAppExpression(entrypoint string, port int, host string) string {
	return fmt.Sprintf("shiny::runApp(%q, port=%d, host=%q)", entrypoint, port, host)
}

// pathForms lists how path can appear in the command line of the R process
// that runs it. RunAppExpression writes it with Go's quoting, which escapes a
// double quote and a backslash; R's front-end script then turns every space
// of the expression into "~+~". The plain forms are kept for processes that
// were started some other way.
func pathForms(path string) []string {
	quoted := strconv.Quote(path)
	quoted = quoted[1 : len(quoted)-1]

	var forms []string
	for _, f := range []string{path, quoted} {
		for _, g := range []string{f, strings.ReplaceAll(f, " ", "~+~")} {
			if !contains(forms, g) {
				forms = append(forms, g)
			}
		}
	}
	return forms
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// inCommandLine reports whether line holds path in any of the forms it
// appears in.
func inCommandLine(line, path string) bool {
	for _, form := range pathForms(path) {
		if strings.Contains(line, form) {
			return true
		}
	}
	return false
}

// procDir is where Linux keeps what it knows of processes; a variable for
// tests.
var procDir = "/proc"

// psLocale is a UTF-8 locale for ps to print in. A daemon may have none (a
// minimal server, cron, a systemd user manager without LANG), and ps then
// prints a character outside ASCII as "?" or "M-CM-)", so a path with an
// accent would never be recognised.
func psLocale() string {
	if runtime.GOOS == "darwin" {
		return "en_US.UTF-8"
	}
	return "C.UTF-8"
}

// psRun runs ps and returns its stdout, in a UTF-8 locale; a package var so
// tests can count and fake the calls.
var psRun = func(args ...string) ([]byte, error) {
	cmd := exec.Command("ps", args...)
	cmd.Env = append(os.Environ(), "LC_ALL="+psLocale())
	return cmd.Output()
}

// procWait is how long a command line that is still empty is waited for,
// and procEvery how often it is read again; variables for tests.
var (
	procWait  = 50 * time.Millisecond
	procEvery = 100 * time.Microsecond
)

// pfKthread is PF_KTHREAD, the flag of a kernel thread, in /proc/<pid>/stat.
const pfKthread = 0x00200000

// procRead is what one look at /proc/<pid> found.
type procRead int

const (
	procGone    procRead = iota // no such process, or a zombie
	procFilled                  // the command line is there
	procPending                 // alive, and the command line still empty
)

// procLook reads /proc/<pid>/cmdline once. The file holds the raw bytes of
// the arguments with a NUL after each: nothing of the locale is involved.
// Right after a fork and exec it is empty for a moment (the kernel lets the
// parent go before the new image's arguments are set up: up to a few hundred
// microseconds), and it stays empty for a zombie; the process's state tells
// the two apart.
func procLook(pid int) (string, procRead) {
	dir := procDir + "/" + strconv.Itoa(pid)
	data, err := os.ReadFile(dir + "/cmdline")
	if err != nil {
		return "", procGone
	}
	if len(data) > 0 {
		return strings.TrimSpace(string(bytes.ReplaceAll(bytes.TrimRight(data, "\x00"), []byte{0}, []byte{' '}))), procFilled
	}
	stat, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return "", procGone
	}
	// "pid (comm) S ...": the name can hold spaces and parentheses, so the
	// state is what follows the last ")".
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) || stat[i+2] == 'Z' || stat[i+2] == 'X' {
		return "", procGone
	}
	// A kernel thread has no command line, ever: not something that is still
	// being set up. Its flags (the 7th field after the name: state, ppid,
	// pgrp, session, tty, tpgid, flags) say so.
	if fields := strings.Fields(string(stat[i+1:])); len(fields) >= 7 {
		if flags, err := strconv.ParseUint(fields[6], 10, 64); err == nil && flags&pfKthread != 0 {
			return "", procGone
		}
	}
	return "", procPending
}

// procCommandLine reads the command line of pid from /proc, waiting briefly
// (procWait) for one that is still being set up. ok says whether /proc could
// be used at all; the line is empty for a process that is gone, a zombie, or
// one whose command line stays empty.
func procCommandLine(pid int) (line string, ok bool) {
	if _, err := os.Stat(procDir + "/self"); err != nil {
		return "", false
	}
	deadline := time.Now().Add(procWait)
	for {
		line, state := procLook(pid)
		if state != procPending || !time.Now().Before(deadline) {
			return line, true
		}
		time.Sleep(procEvery)
	}
}

// commandLine returns the command line of the process at pid, read the same
// way whatever the locale: from /proc on Linux, otherwise from ps in a UTF-8
// locale. (macOS has no /proc; its kern.procargs2 would avoid the process
// but needs a dependency for one call, and ps with the locale set prints the
// same text.)
func commandLine(pid int) (string, bool) {
	if line, ok := procCommandLine(pid); ok {
		return line, line != ""
	}
	out, err := psRun("-p", strconv.Itoa(pid), "-o", "command=")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
