//go:build !linux && !darwin

package process

func openExitHandle(pid int) (exitHandle, error) { return nil, ErrWatchUnsupported }
