package process

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
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

// socketLockWait is how long ListenUnix waits for another start-up to finish
// its part. A variable so a test can shorten it.
var socketLockWait = 2 * time.Second

// ListenUnix opens a Unix domain socket listener at path. If a socket file
// already exists there but nothing is actually listening on it (e.g. left
// behind by a crash), it's removed and replaced. If something genuinely is
// listening, ListenUnix refuses to start a second instance rather than
// silently taking over — two shinyctl-api processes racing on state.json
// is exactly the single-writer problem this whole design exists to avoid.
func ListenUnix(path string) (net.Listener, error) {
	// Two starting at once must not interleave the check, the removal and the
	// listen: one could remove the socket the other had just bound. The lock
	// is held for that part only (milliseconds), and the kernel drops it with
	// the process. A holder that stays longer than socketLockWait is stuck:
	// say so, instead of waiting for ever and starting later than the command
	// that started us reported.
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(path), "api.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the start-up lock: %w", err)
	}
	defer lock.Close()
	deadline := time.Now().Add(socketLockWait)
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("locking the start-up lock: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("another shinyctl-api is starting (%s is locked); try again in a moment", lock.Name())
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

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
