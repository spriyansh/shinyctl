package process

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// measure is the platform's own measurement of each process; a variable for
// the tests.
var measure = platformMemory

// ExactMemory returns the platform's own measure of each of pids, in bytes,
// and never asks ps. A PID it could not measure is absent from the result. The
// error says why it could not run at all, and after a timeout it wraps
// context.DeadlineExceeded; whoever enforces a limit on it must not fall back
// to a number of another kind.
func ExactMemory(pids []int) (map[int]int64, error) {
	if len(pids) == 0 {
		return nil, nil
	}
	return measure(pids)
}

// Memory is what status shows: ExactMemory, with ps's resident size standing
// in for every PID the platform could not measure, in one ps call. After a
// timeout there is no ps and no result: a machine that makes the platform's
// tool take seconds is swapping, and the resident size is then the one number
// known to be wrong, so those PIDs are left out and shown as unknown.
//
// The error, when not nil, says what was stood in for or left out, for a log
// line; the map is still usable.
func Memory(pids []int) (map[int]int64, error) {
	if len(pids) == 0 {
		return nil, nil
	}

	got, err := measure(pids)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w; memory of pid %s left out", err, pidList(pids))
	}

	var missing []int
	for _, pid := range pids {
		if _, ok := got[pid]; !ok {
			missing = append(missing, pid)
		}
	}
	if len(missing) == 0 {
		return got, err
	}

	result := make(map[int]int64, len(pids))
	for pid, size := range got {
		result[pid] = size
	}

	// A PID that ps doesn't know either exited a moment ago, and is not
	// worth a line in the log.
	var stoodIn []int
	for pid, size := range RSS(missing) {
		result[pid] = size
		stoodIn = append(stoodIn, pid)
	}
	if len(stoodIn) == 0 {
		return result, err
	}
	list := pidList(stoodIn)

	if err != nil {
		return result, fmt.Errorf("%w; ps's resident size stands in for pid %s", err, list)
	}
	return result, fmt.Errorf("ps's resident size stands in for pid %s", list)
}

// pidList names pids in numeric order, for an error message.
func pidList(pids []int) string {
	sorted := slices.Clone(pids)
	slices.Sort(sorted)
	names := make([]string, len(sorted))
	for i, pid := range sorted {
		names[i] = strconv.Itoa(pid)
	}
	return strings.Join(names, ", ")
}
