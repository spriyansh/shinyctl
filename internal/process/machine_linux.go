package process

// platformMachineMemory reads /proc/meminfo and, where the kernel has it,
// /proc/pressure/memory.
func platformMachineMemory() (MachineMemory, error) {
	return readMachineFromProc(procDir)
}
