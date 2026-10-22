package process

import (
	"os/exec"
	"strconv"
	"strings"
)

// psOutput runs ps and returns its stdout, as a package var so tests can
// count the calls.
var psOutput = func(args ...string) ([]byte, error) {
	return exec.Command("ps", args...).Output()
}

// RSS returns the resident memory, in bytes, of each of pids that ps can
// see. One ps call covers all of them: status asks for every app at once,
// and a process spawn per app on every query would add up. A PID that is
// gone is simply absent from the result, and so is every PID if ps itself
// fails — memory is informational, never worth failing a status over.
func RSS(pids []int) map[int]int64 {
	if len(pids) == 0 {
		return nil
	}

	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}

	// Output still returns what ps printed when it exits non-zero, which
	// it does when some of the PIDs no longer exist.
	out, _ := psOutput("-o", "pid=,rss=", "-p", strings.Join(list, ","))
	return parseRSS(out)
}

// parseRSS reads `ps -o pid=,rss=` output: one "PID RSS" line per process,
// RSS in KiB on both macOS and procps. Lines that don't parse are skipped.
func parseRSS(out []byte) map[int]int64 {
	result := make(map[int]int64)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		result[pid] = kib * 1024
	}
	return result
}
