package process

import (
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// parseMeminfo reads the memory lines of a /proc/meminfo, which the kernel
// prints in kB of 1024 bytes. Without MemTotal and MemAvailable (the latter
// is from kernel 3.14) there is nothing to say. SwapTotal and SwapFree are
// missing from a kernel built without swap, which then has none.
func parseMeminfo(data []byte) (MachineMemory, error) {
	var total, avail, swapTotal, swapFree int64
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "MemTotal":
			total, haveTotal = kb*1024, true
		case "MemAvailable":
			avail, haveAvail = kb*1024, true
		case "SwapTotal":
			swapTotal = kb * 1024
		case "SwapFree":
			swapFree = kb * 1024
		}
	}
	if !haveTotal || total <= 0 {
		return MachineMemory{}, fmt.Errorf("no MemTotal in meminfo")
	}
	if !haveAvail {
		return MachineMemory{}, fmt.Errorf("no MemAvailable in meminfo (kernel older than 3.14)")
	}
	m := MachineMemory{TotalBytes: total, AvailableBytes: avail}
	if swapTotal > 0 {
		m.SwapTotalBytes = swapTotal
		m.SwapUsedBytes = max(swapTotal-swapFree, 0)
	}
	return m, nil
}

// parsePressure reads avg60 of the "some" line of /proc/pressure/memory: the
// percentage of the last minute during which at least one task waited for
// memory.
func parsePressure(data []byte) (some60 float64, ok bool) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, f := range fields[1:] {
			if v, found := strings.CutPrefix(f, "avg60="); found {
				p, err := strconv.ParseFloat(v, 64)
				if err != nil || p < 0 {
					return 0, false
				}
				return p, true
			}
		}
		return 0, false
	}
	return 0, false
}

// readMachineFromProc reads meminfo and, if it can, pressure/memory from a
// proc directory. The pressure file is missing on kernels without the
// feature or booted with psi=0, and may fail to read in other ways; none of
// that fails the reading, the stall is just not known.
func readMachineFromProc(dir string) (MachineMemory, error) {
	data, err := os.ReadFile(dir + "/meminfo")
	if err != nil {
		return MachineMemory{}, err
	}
	m, err := parseMeminfo(data)
	if err != nil {
		return MachineMemory{}, err
	}
	if p, err := os.ReadFile(dir + "/pressure/memory"); err == nil {
		m.StallPercent, m.StallKnown = parsePressure(p)
	}
	return m, nil
}

// machineFromSysctl builds the reading from macOS's values: the pressure
// level, the free percentage (0 when unknown), the size of memory and the raw
// vm.swapusage (xsw_usage: total, avail and used as 64-bit numbers, then the
// page size and a flag of 4 bytes each; 32 bytes). A swapusage of another
// length leaves swap at zero.
func machineFromSysctl(pressure, level uint32, memsize uint64, swapusage []byte) MachineMemory {
	m := MachineMemory{
		TotalBytes: int64(memsize),
		Pressure:   pressureName(pressure),
	}
	if level > 100 {
		level = 100
	}
	m.AvailableBytes = int64(memsize * uint64(level) / 100)
	if len(swapusage) == 32 {
		m.SwapTotalBytes = int64(binary.NativeEndian.Uint64(swapusage[0:8]))
		m.SwapUsedBytes = int64(binary.NativeEndian.Uint64(swapusage[16:24]))
	}
	return m
}

// pressureName is the name of the kernel's memory pressure level: 1 normal,
// 2 warning, 4 critical.
func pressureName(level uint32) string {
	switch level {
	case 1:
		return "normal"
	case 2:
		return "warning"
	case 4:
		return "critical"
	}
	return "unknown"
}
