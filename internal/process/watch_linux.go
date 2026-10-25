package process

import (
	"errors"

	"golang.org/x/sys/unix"
)

type pidfd int

// openExitHandle opens a pidfd, which becomes readable when the process
// exits (Linux 5.3 and later; older kernels answer ENOSYS).
func openExitHandle(pid int) (exitHandle, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil, ErrNotOurs
	}
	if err != nil {
		return nil, err
	}
	return pidfd(fd), nil
}

func (fd pidfd) wait() {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if _, err := unix.Poll(fds, -1); err == unix.EINTR {
			continue
		}
		// Readable is the exit. Any other error ends the wait too: the
		// caller then looks, and the look is cheap.
		return
	}
}

func (fd pidfd) close() { unix.Close(int(fd)) }
