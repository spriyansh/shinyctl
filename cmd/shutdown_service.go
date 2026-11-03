/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// serviceName is the unit that "shinyctl startup" tells the user to install.
const serviceName = "shinyctl"

// serviceEnv is what stopService reads from the machine, so tests can supply
// it, as startupEnv does.
type serviceEnv struct {
	goos     string
	uid      int
	getenv   func(string) string
	exists   func(string) bool
	lookPath func(string) (string, error)

	// run runs systemctl with args and extra environment entries, and returns
	// its standard output whatever its exit status: is-active exits non-zero
	// for a unit that is not active, and says so on stdout.
	run func(env []string, args ...string) (stdout string, err error)
}

func realServiceEnv() serviceEnv {
	return serviceEnv{
		goos:     runtime.GOOS,
		uid:      os.Getuid(),
		getenv:   os.Getenv,
		exists:   func(path string) bool { _, err := os.Stat(path); return err == nil },
		lookPath: exec.LookPath,
		run:      runSystemctl,
	}
}

// runSystemctl runs systemctl with env added to the environment. A stop gets
// 30 seconds, anything else 5: a bus that does not answer must not hold the
// command up.
func runSystemctl(env []string, args ...string) (string, error) {
	timeout := 5 * time.Second
	if slices.Contains(args, "stop") {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = time.Second // and not for a child of it that holds the pipe
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return string(out), fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// stopService stops the shinyctl systemd user service when it is running, and
// says whether it did. Without it the service would start shinyctl-api again
// two seconds after it exits, and with it every app. The unit is stopped, not
// disabled. Anything it can't tell (no systemctl, no bus) is not an error:
// the daemon refuses to shut down when it is a service's main process, which
// is the net under this.
func stopService(env serviceEnv) (bool, error) {
	if env.goos != "linux" {
		return false, nil
	}
	if _, err := env.lookPath("systemctl"); err != nil {
		return false, nil
	}

	// After sudo -iu or su - the session has no runtime directory in its
	// environment, though the user's manager (lingering) is there.
	var extra []string
	if env.getenv("XDG_RUNTIME_DIR") == "" {
		dir := fmt.Sprintf("/run/user/%d", env.uid)
		if env.exists(dir + "/bus") {
			extra = []string{"XDG_RUNTIME_DIR=" + dir}
		}
	}

	out, _ := env.run(extra, "--user", "is-active", serviceName)
	switch strings.TrimSpace(out) {
	case "active", "activating", "deactivating", "reloading":
	default:
		return false, nil
	}

	if _, err := env.run(extra, "--user", "stop", serviceName); err != nil {
		return false, err
	}
	return true, nil
}

var userManagerUnit = regexp.MustCompile(`^user@[0-9]+\.service$`)

// ownServiceUnit is the systemd service this process is the main process of,
// and whether it is a user service; "" when it is not one.
func ownServiceUnit() (unit string, user bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	cgroup, _ := os.ReadFile("/proc/self/cgroup")
	return serviceUnit(os.Getenv, string(cgroup), os.Getpid())
}

// serviceUnit is ownServiceUnit with what it reads as parameters: the
// environment, the text of /proc/self/cgroup and the process's pid.
//
// From systemd 248 SYSTEMD_EXEC_PID names the main process of the unit, which
// settles it. Before that, INVOCATION_ID is set for every process of a unit
// (and leaks into shells started from one), so it only counts together with
// the process sitting in a user service whose name contains shinyctl: a
// daemon started by hand from a shell inside code-server.service is not
// shinyctl's service, and must not be told to stop that one.
func serviceUnit(getenv func(string) string, cgroup string, pid int) (unit string, user bool) {
	elements := strings.Split(strings.Trim(cgroupPath(cgroup), "/"), "/")
	inUserManager := slices.ContainsFunc(elements, userManagerUnit.MatchString)

	if execPID := getenv("SYSTEMD_EXEC_PID"); execPID != "" {
		if n, err := strconv.Atoi(execPID); err != nil || n != pid {
			return "", false
		}
		unit = serviceName + ".service"
		for _, e := range slices.Backward(elements) {
			if strings.HasSuffix(e, ".service") && !userManagerUnit.MatchString(e) {
				unit = e
				break
			}
		}
		return unit, inUserManager
	}

	if getenv("INVOCATION_ID") == "" || !inUserManager {
		return "", false
	}
	last := elements[len(elements)-1]
	if strings.HasSuffix(last, ".service") && strings.Contains(last, serviceName) && !userManagerUnit.MatchString(last) {
		return last, true
	}
	return "", false
}

// cgroupPath is the control group in the text of /proc/self/cgroup: the
// name=systemd line of the older and hybrid layouts, which is where systemd
// puts the process (the "0::" line of a hybrid system is just the root), or
// else the unified hierarchy's "0::" line.
func cgroupPath(text string) string {
	var unified string
	for _, line := range strings.Split(text, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch {
		case slices.Contains(strings.Split(parts[1], ","), "name=systemd"):
			return parts[2]
		case parts[0] == "0" && parts[1] == "":
			unified = parts[2]
		}
	}
	return unified
}
