package process

import (
	"fmt"
	"os"
	"time"
)

// EnsureAPIRunning makes sure shinyctl-api is reachable over its Unix
// socket, launching it detached in the background (the ssh-agent pattern)
// if it isn't already. It returns once the socket is actually dialable, or
// an error if it never comes up.
func EnsureAPIRunning() error {
	socketPath, err := SocketPath()
	if err != nil {
		return err
	}

	if SocketDialable(socketPath) {
		return nil
	}

	// shinyctl-api would exit on a home directory that is too long, and all
	// this side could say is that it did not come up.
	if err := CheckSocketPaths(); err != nil {
		return err
	}

	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	logPath, err := LogPath()
	if err != nil {
		return err
	}

	if _, err := LaunchDetached(exePath, []string{"serve"}, logPath); err != nil {
		return fmt.Errorf("starting shinyctl-api: %w", err)
	}

	for i := 0; i < 20; i++ {
		if SocketDialable(socketPath) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("shinyctl-api did not come up within 1s, check %s", logPath)
}
