package process

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// platformMachineMemory reads four sysctls. The pressure level and the size
// of memory must be there; the free percentage (undocumented, it is what
// memory_pressure prints) and the swap usage only add to what is shown, so
// without them they are left at zero.
func platformMachineMemory() (MachineMemory, error) {
	pressure, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return MachineMemory{}, fmt.Errorf("kern.memorystatus_vm_pressure_level: %w", err)
	}
	memsize, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return MachineMemory{}, fmt.Errorf("hw.memsize: %w", err)
	}
	level, _ := unix.SysctlUint32("kern.memorystatus_level")
	swap, _ := unix.SysctlRaw("vm.swapusage")
	return machineFromSysctl(pressure, level, memsize, swap), nil
}
