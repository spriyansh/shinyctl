//go:build !linux && !darwin

package process

// Where neither /proc nor the macOS sysctls exist, the machine's memory is
// not read.
func platformMachineMemory() (MachineMemory, error) {
	return MachineMemory{}, ErrNoMachineMemory
}
