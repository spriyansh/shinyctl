package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// footprintPath is where macOS keeps footprint; a variable for the tests.
var footprintPath = "/usr/bin/footprint"

// footprintTimeout bounds one measurement: footprint takes some tens of
// milliseconds, and status asks for it on every query.
var footprintTimeout = lsofTimeout

// footprintRun runs footprint and returns what it printed; a variable so that
// the tests can stand in for it.
var footprintRun = runFootprint

// platformMemory is each process's footprint: the number Activity Monitor
// shows as Memory. Unlike the resident size from ps it counts what macOS
// compressed or swapped out of a process, which is most of a process once the
// machine is short of memory, so it doesn't shrink by itself under pressure.
//
// One call covers all of pids. footprint prints a block per process it can
// read and a message on stderr for each one it can't, and exits non-zero only
// when it read none, so a PID is simply absent when it could not be measured.
// Each PID is behind its own --pid: a bare number is taken for a name to
// match. It reads the process through its task port and never stops it.
func platformMemory(pids []int) (map[int]int64, error) {
	args := []string{"-f", "bytes"}
	want := make(map[int]bool, len(pids))
	for _, pid := range pids {
		args = append(args, "--pid", strconv.Itoa(pid))
		want[pid] = true
	}

	out, err := footprintRun(args...)
	if err != nil {
		// A non-zero exit still comes with the blocks it did read. Not
		// starting, or being cut off, comes with nothing worth reading.
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, err
		}
	}
	return parseFootprint(out, want), nil
}

// runFootprint runs footprint with a fixed environment, as run does for lsof.
func runFootprint(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), footprintTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, footprintPath, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	cmd.WaitDelay = time.Second // and not for a child of it that holds the pipe
	out, err := cmd.Output()

	// A command the context killed reports the kill as an *exec.ExitError
	// ("signal: killed"), which is indistinguishable from footprint failing
	// on its own; so the deadline is asked of the context, before anything
	// is made of the error.
	if ctx.Err() != nil {
		return out, fmt.Errorf("footprint did not finish within %s: %w", footprintTimeout, ctx.Err())
	}
	return out, err
}

var (
	// footprintHeader opens a process's block, on a line that starts in the
	// first column: `zsh [92107]: 64-bit    Footprint: 1655120 B (16384 ...`.
	// The name may hold spaces and brackets; the Summary line has no PID.
	footprintHeader = regexp.MustCompile(`^\S.*\[(\d+)\]: .*Footprint: (\d+) B`)

	// footprintLedger is the kernel's own count in the block's auxiliary
	// data, which is what top and Activity Monitor show. The header's
	// number is only the sum of the dirty regions.
	footprintLedger = regexp.MustCompile(`^\s+phys_footprint: (\d+) B`)
)

// parseFootprint reads footprint's default output, in bytes, for the PIDs in
// want. The blocks come sorted by size, not in the order asked. A block's
// phys_footprint wins over the number in its header, which is used when the
// block has none. Anything else is skipped, so the usage text footprint
// prints for an option it doesn't know gives an empty result.
func parseFootprint(out []byte, want map[int]bool) map[int]int64 {
	result := make(map[int]int64)
	current := 0 // the PID of the block being read, or 0 for none we want
	for _, line := range bytes.Split(out, []byte("\n")) {
		if m := footprintHeader.FindSubmatch(line); m != nil {
			pid, _ := strconv.Atoi(string(m[1]))
			size, err := strconv.ParseInt(string(m[2]), 10, 64)
			current = 0
			if err == nil && want[pid] {
				current = pid
				result[pid] = size
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("Summary")) {
			current = 0
			continue
		}
		if m := footprintLedger.FindSubmatch(line); m != nil && current != 0 {
			if size, err := strconv.ParseInt(string(m[1]), 10, 64); err == nil {
				result[current] = size
			}
		}
	}
	return result
}
