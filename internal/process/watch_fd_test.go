package process

import "syscall"

func fcntlGetFD(fd int) (int, error) {
	r, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}
