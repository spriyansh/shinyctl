package process

import "golang.org/x/sys/unix"

// kinfoProcs reads the kernel's process list; a variable for the tests.
var kinfoProcs = func() ([]unix.KinfoProc, error) {
	return unix.SysctlKinfoProcSlice("kern.proc.all")
}

// sZomb is SZOMB, the p_stat of a process that has exited and is not yet
// reaped.
const sZomb = 5

func platformTable() ([]procInfo, error) {
	procs, err := kinfoProcs()
	if err != nil {
		return nil, err
	}
	table := make([]procInfo, 0, len(procs))
	for _, k := range procs {
		table = append(table, procInfo{
			pid:    int(k.Proc.P_pid),
			ppid:   int(k.Eproc.Ppid),
			pgid:   int(k.Eproc.Pgid),
			start:  uint64(k.Proc.P_starttime.Sec)*1_000_000 + uint64(k.Proc.P_starttime.Usec),
			zombie: k.Proc.P_stat == sZomb,
		})
	}
	return table, nil
}
