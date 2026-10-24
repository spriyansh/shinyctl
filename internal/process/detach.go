package process

import (
	"os"
	"os/exec"
	"syscall"
)

// StartDetached starts path with args as a background process, fully
// detached from the caller: a new session (via setsid), so it survives the
// caller's process group and controlling terminal going away, with its
// output appended to logPath instead of inherited from the caller. It does
// not wait for the child to exit; the caller can, through the returned
// command.
func StartDetached(path string, args []string, logPath string) (*exec.Cmd, error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	// The child holds its own copy of the descriptor; the caller's would
	// only leak one per start.
	defer logFile.Close()

	cmd := exec.Command(path, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return cmd, nil
}

// LaunchDetached is StartDetached for a caller that has no use for the
// process afterwards, and wants only its PID.
func LaunchDetached(path string, args []string, logPath string) (int, error) {
	cmd, err := StartDetached(path, args, logPath)
	if err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}
