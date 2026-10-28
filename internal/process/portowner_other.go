//go:build !linux && !darwin

package process

// Where neither /proc nor lsof is known, the owner can't be told, and a copy
// is ready when its port accepts connections, as it has always been.
func portOwner(pid, port int) Owner { return OwnerUnknown }
