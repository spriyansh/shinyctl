package process

import (
	"os"
	"strconv"
	"strings"
)

func platformTable() ([]procInfo, error) { return tableIn(procDir) }

// tableIn reads the process table from the /proc at root (a variable of the
// tests' own, like procDir). A process whose stat file can't be read exited
// while the directory was being listed, and is skipped.
func tableIn(root string) ([]procInfo, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var table []procInfo
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(root + "/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		if p, ok := parseStat(n, string(data)); ok {
			table = append(table, p)
		}
	}
	return table, nil
}

// parseStat reads a /proc/<pid>/stat line. The fields that matter come after
// the command name, which is in parentheses and may hold spaces and
// parentheses of its own, so they are counted from the last ")": the state
// (field 3 of proc(5); Z and X are a zombie), the parent (4), the process
// group (5) and the start time (22).
func parseStat(pid int, line string) (procInfo, bool) {
	i := strings.LastIndexByte(line, ')')
	if i < 0 {
		return procInfo{}, false
	}
	f := strings.Fields(line[i+1:])
	if len(f) < 20 {
		return procInfo{}, false
	}
	ppid, e1 := strconv.Atoi(f[1])
	pgid, e2 := strconv.Atoi(f[2])
	start, e3 := strconv.ParseUint(f[19], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return procInfo{}, false
	}
	return procInfo{pid: pid, ppid: ppid, pgid: pgid, start: start, zombie: f[0] == "Z" || f[0] == "X"}, true
}
