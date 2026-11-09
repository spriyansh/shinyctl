package process

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// measureTrees is the platform's own measurement of each copy's processes; a
// variable for the tests.
var measureTrees = platformTreeMemory

// CopyTrees returns the processes of each copy led by one of leaders, leader
// first: its process group and what descends from it (see copyProcs), from one
// read of the process table. Every leader has an entry, a copy whose leader is
// not in the table (it just exited) with itself alone. The error says the
// table could not be read; the caller then measures each leader alone, which
// is what it did before copies had processes of their own.
func CopyTrees(leaders []int) (map[int][]int, error) {
	if len(leaders) == 0 {
		return nil, nil
	}
	table, err := readTable()
	if err != nil {
		return nil, err
	}
	pgid := make(map[int]int, len(table))
	for _, p := range table {
		pgid[p.pid] = p.pgid
	}
	trees := make(map[int][]int, len(leaders))
	for _, leader := range leaders {
		// A copy leads its group when the table says so; a record from a
		// daemon before groups is in the daemon's, and has only what descends
		// from it.
		inGroup, outside := copyProcs(table, leader, pgid[leader] == leader)
		tree := []int{leader}
		for _, p := range append(inGroup, outside...) {
			if p.pid != leader {
				tree = append(tree, p.pid)
			}
		}
		trees[leader] = tree
	}
	return trees, nil
}

// ExactTreeMemory returns the platform's own measure of each copy, in bytes:
// the sum over its processes, never asking ps. A tree is keyed by its first
// pid, the leader, and is absent when the leader could not be measured; a
// process of it that could not be measured (it exited a moment ago) is left
// out of the sum. The error says why nothing could be measured at all, and
// after a timeout it wraps context.DeadlineExceeded; whoever enforces a limit
// on it must not fall back to a number of another kind.
//
// Linux counts a process's proportional share of every page (Pss and
// SwapPss), so pages that a forked worker shares with its parent are counted
// once between them. macOS asks footprint for the de-duplicated total of a
// copy that has workers.
func ExactTreeMemory(trees [][]int) (map[int]int64, error) {
	trees = nonEmpty(trees)
	if len(trees) == 0 {
		return nil, nil
	}
	return measureTrees(trees)
}

// TreeMemory is what status shows: ExactTreeMemory, with ps's resident size of
// the leader standing in for every copy the platform could not measure, in one
// ps call. After a timeout there is no ps and no result: a machine that makes
// the platform's tool take seconds is swapping, and the resident size is then
// the one number known to be wrong, so those copies are left out and shown as
// unknown.
//
// The error, when not nil, says what was stood in for or left out, for a log
// line; the map is still usable.
func TreeMemory(trees [][]int) (map[int]int64, error) {
	trees = nonEmpty(trees)
	if len(trees) == 0 {
		return nil, nil
	}
	leaders := make([]int, len(trees))
	for i, tree := range trees {
		leaders[i] = tree[0]
	}

	got, err := measureTrees(trees)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w; memory of pid %s left out", err, pidList(leaders))
	}

	var missing []int
	for _, pid := range leaders {
		if _, ok := got[pid]; !ok {
			missing = append(missing, pid)
		}
	}
	if len(missing) == 0 {
		return got, err
	}

	result := make(map[int]int64, len(leaders))
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

func nonEmpty(trees [][]int) [][]int {
	var out [][]int
	for _, tree := range trees {
		if len(tree) > 0 {
			out = append(out, tree)
		}
	}
	return out
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
