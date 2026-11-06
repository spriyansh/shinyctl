package process

import (
	"os"
	"strconv"
	"strings"
)

// platformTreeMemory is the memory of each copy: the sum over its processes
// of platformMemory. A copy whose leader can't be read is absent, and a
// process of it that can't be read is left out of the sum.
func platformTreeMemory(trees [][]int) (map[int]int64, error) {
	var all []int
	for _, tree := range trees {
		all = append(all, tree...)
	}
	sizes, err := platformMemory(all)
	if err != nil {
		return nil, err
	}
	result := make(map[int]int64, len(trees))
	for _, tree := range trees {
		if _, ok := sizes[tree[0]]; !ok {
			continue
		}
		var sum int64
		for _, pid := range tree {
			sum += sizes[pid] // 0 for one that could not be read
		}
		result[tree[0]] = sum
	}
	return result, nil
}

// platformMemory is the memory of each process, from /proc: its proportional
// share of every page, in memory and swapped out (Pss and SwapPss of
// smaps_rollup), so that the pages a worker forked from R shares with it are
// counted once between them and not once each. Where smaps_rollup can't be
// read (a kernel before 4.14, a process that is not dumpable or setuid), the
// resident and the swapped-out memory of status stand in for that process.
// A process whose pages the kernel moved to swap, or to zram, would otherwise
// look small exactly when the machine is short of memory. It is not the same
// kind of number as macOS's footprint: mapped files count, in proportion.
//
// A PID that can't be read is absent: it exited, or it is a zombie, which has
// no memory lines. There is no error, only absences, and ps stands in for
// them.
func platformMemory(pids []int) (map[int]int64, error) {
	result := make(map[int]int64, len(pids))
	for _, pid := range pids {
		dir := procDir + "/" + strconv.Itoa(pid)
		if data, err := os.ReadFile(dir + "/smaps_rollup"); err == nil {
			if size, ok := parseSmapsRollup(data); ok {
				result[pid] = size
				continue
			}
		}
		data, err := os.ReadFile(dir + "/status")
		if err != nil {
			continue
		}
		if size, ok := parseProcStatus(data); ok {
			result[pid] = size
		}
	}
	return result, nil
}

// parseSmapsRollup adds Pss and SwapPss of a /proc/<pid>/smaps_rollup, which
// the kernel prints in kB of 1024 bytes. Without Pss there is nothing to
// measure (the file of a kernel thread or a zombie is empty); without SwapPss
// (a kernel built without swap) nothing is swapped. The lines Pss_Anon,
// Pss_File and Pss_Shmem are parts of Pss and not added again.
func parseSmapsRollup(data []byte) (int64, bool) {
	var proportional, swapped int64
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || (name != "Pss" && name != "SwapPss") {
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
		if name == "Pss" {
			proportional, found = kib*1024, true
		} else {
			swapped = kib * 1024
		}
	}
	if !found {
		return 0, false
	}
	return proportional + swapped, true
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
