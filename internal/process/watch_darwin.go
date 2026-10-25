package process

import (
	"errors"

	"golang.org/x/sys/unix"
)

type kqueueFD int

// openExitHandle registers for the process's exit with a kqueue.
func openExitHandle(pid int) (exitHandle, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	var ev unix.Kevent_t
	unix.SetKevent(&ev, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	ev.Fflags = unix.NOTE_EXIT
	for {
		_, err = unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		unix.Close(kq)
		if errors.Is(err, unix.ESRCH) {
			return nil, ErrNotOurs
		}
		return nil, err
	}
	return kqueueFD(kq), nil
}

func (kq kqueueFD) wait() {
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(int(kq), nil, events, nil)
		if err == unix.EINTR || (err == nil && n == 0) {
			continue
		}
		return // an exit, or an error that ends the wait; the caller looks
	}
}

func (kq kqueueFD) close() { unix.Close(int(kq)) }
