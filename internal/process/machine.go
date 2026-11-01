package process

import "errors"

// MachineMemory is the memory of the whole machine, not of any process.
// AvailableBytes is what can be had without swapping: MemAvailable on Linux;
// on macOS it is derived from the kernel's free percentage, and the swap
// total is the size of the swap files, which macOS adds as needed.
type MachineMemory struct {
	TotalBytes     int64
	AvailableBytes int64
	SwapTotalBytes int64
	SwapUsedBytes  int64

	// StallPercent is how much of the last minute at least one process was
	// waiting for memory (some avg60 of the pressure stall information);
	// only meaningful when StallKnown. Linux only.
	StallPercent float64
	StallKnown   bool

	// Pressure is the kernel's own level on macOS (normal, warning or
	// critical); empty on Linux.
	Pressure string
}

// ErrNoMachineMemory says this system has no way to read the machine's
// memory.
var ErrNoMachineMemory = errors.New("the machine's memory can't be read on this system")

// ReadMachineMemory reads the memory of the whole machine. It is a few small
// reads or system calls and may be called every second.
func ReadMachineMemory() (MachineMemory, error) {
	return platformMachineMemory()
}
