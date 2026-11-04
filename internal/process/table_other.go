//go:build !linux && !darwin

package process

import "errors"

// Where neither /proc nor the kernel's process list is known there is no
// table; callers fall back to the copy's leader alone, which is what they did
// before copies had processes of their own.
func platformTable() ([]procInfo, error) {
	return nil, errors.New("the process table can't be read on this system")
}
