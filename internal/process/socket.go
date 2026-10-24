package process

import (
	"fmt"
	"net"
	"os"
)

// MaxSocketPath is the longest Unix socket path that can be bound on every
// supported system: sockaddr_un.sun_path is 104 bytes on macOS including
// the terminating NUL (108 on Linux).
const MaxSocketPath = 103

// CheckSocketPath returns a clear error for a socket path that is too long
// to bind, instead of the OS's "invalid argument". The paths are under the
// home directory, which the user chose.
func CheckSocketPath(path string) error {
	if len(path) > MaxSocketPath {
		return fmt.Errorf("%s is too long for a socket (%d bytes, at most %d) — use a shorter home directory", path, len(path), MaxSocketPath)
	}
	return nil
}

// ListenUnix opens a Unix domain socket listener at path. If a socket file
// already exists there but nothing is actually listening on it (e.g. left
// behind by a crash), it's removed and replaced. If something genuinely is
// listening, ListenUnix refuses to start a second instance rather than
// silently taking over — two shinyctl-api processes racing on state.json
// is exactly the single-writer problem this whole design exists to avoid.
func ListenUnix(path string) (net.Listener, error) {
	if SocketDialable(path) {
		return nil, fmt.Errorf("shinyctl-api already running (socket %s is live)", path)
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale socket: %w", err)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}

	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}

	return listener, nil
}

// SocketDialable reports whether something is actually listening at path
// right now.
func SocketDialable(path string) bool {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
