package process

import (
	"fmt"
	"net"
	"os"
)

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
