package process

import "slices"

// procInfo is one process of the table: what is needed to tell a copy's
// processes from everyone else's. start identifies the process across a reused
// pid: the kernel's own start time (clock ticks since boot on Linux,
// microseconds since the epoch on macOS), which only ever compares equal to
// itself.
type procInfo struct {
	pid, ppid, pgid int
	start           uint64
	zombie          bool
}

// readTable reads the system's process table in-process: no ps, no fork, about
// a millisecond for hundreds of processes on macOS (ps takes thirty). A
// variable of its own, so that the tests can make the table fail without
// touching what the identity checks read.
var readTable = platformTable

// copyProcs returns the live processes of the copy led by leader, from a
// table. The first result is the copy's group: with group, every live process
// of process group leader, the leader first; without (a record from a daemon
// that did not start copies in groups of their own), only the leader. The
// second result is everything outside that group that descends, by parent,
// from one of its processes: a callr worker, which calls setsid, or a child of
// a worker that was re-parented to init. Descendants are walked from every
// member, not only from the leader, so a re-parented worker's own children are
// found too. Zombies are never listed: they hold nothing and cannot be
// signalled to any effect.
func copyProcs(table []procInfo, leader int, group bool) (inGroup, outside []procInfo) {
	byPID := make(map[int]procInfo, len(table))
	children := map[int][]procInfo{}
	for _, p := range table {
		byPID[p.pid] = p
		if !p.zombie {
			children[p.ppid] = append(children[p.ppid], p)
		}
	}

	if me, ok := byPID[leader]; ok && !me.zombie {
		inGroup = append(inGroup, me)
	}
	if group {
		for _, p := range table {
			if p.pgid == leader && !p.zombie && p.pid != leader {
				inGroup = append(inGroup, p)
			}
		}
	}

	seen := make(map[int]bool, len(inGroup))
	for _, p := range inGroup {
		seen[p.pid] = true
	}
	queue := slices.Clone(inGroup)
	for i := 0; i < len(queue); i++ {
		for _, c := range children[queue[i].pid] {
			if seen[c.pid] {
				continue
			}
			seen[c.pid] = true
			outside = append(outside, c)
			queue = append(queue, c)
		}
	}
	return inGroup, outside
}

// parents maps every process of a table to its parent, for portOwner.
func parents(table []procInfo) map[int]int {
	parent := make(map[int]int, len(table))
	for _, p := range table {
		parent[p.pid] = p.ppid
	}
	return parent
}
