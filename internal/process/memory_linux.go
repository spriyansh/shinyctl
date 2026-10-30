package process

import (
	"os"
	"strconv"
	"strings"
)

// platformMemory is the resident and the swapped-out memory of each process,
// from /proc. A process whose pages the kernel moved to swap, or to zram,
// would otherwise look small to ps exactly when the machine is short of
// memory. It is not the same kind of number as macOS's footprint: shared
// pages and mapped files count in the resident part.
//
// A PID that can't be read is absent: it exited, or it is a zombie, which has
// no memory lines. There is no error, only absences, and ps stands in for
// them.
func platformMemory(pids []int) (map[int]int64, error) {
	result := make(map[int]int64, len(pids))
	for _, pid := range pids {
		data, err := os.ReadFile(procDir + "/" + strconv.Itoa(pid) + "/status")
		if err != nil {
			continue
		}
		if size, ok := parseProcStatus(data); ok {
			result[pid] = size
		}
	}
	return result, nil
}

// parseProcStatus adds VmRSS and VmSwap of a /proc/<pid>/status, which the
// kernel prints in kB of 1024 bytes. Without VmRSS (a zombie, a kernel
// thread) there is nothing to measure; without VmSwap (a kernel built without
// swap) nothing is swapped.
func parseProcStatus(data []byte) (int64, bool) {
	var resident, swapped int64
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || (name != "VmRSS" && name != "VmSwap") {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) != 2 || fields[1] != "kB" {
			continue
		}
		kib, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		if name == "VmRSS" {
			resident, found = kib*1024, true
		} else {
			swapped = kib * 1024
		}
	}
	if !found {
		return 0, false
	}
	return resident + swapped, true
}
