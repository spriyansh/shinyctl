package process

import (
	"os"
	"os/exec"
	"syscall"
)

// LaunchDetached starts path with args as a background process, fully
// detached from the caller: a new session (via setsid), so it survives the
// caller's process group and controlling terminal going away, with its
// output redirected to logPath instead of inherited from the caller. It
// does not wait for the child to exit.
func LaunchDetached(path string, args []string, logPath string) (int, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return 0, err
	}

	cmd := exec.Command(path, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return 0, err
	}

	return cmd.Process.Pid, nil
}
