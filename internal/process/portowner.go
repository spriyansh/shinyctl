package process

import (
	"bufio"
	"strconv"
	"strings"
)

// Owner says whose a listening socket is.
type Owner int

const (
	// OwnerUnknown: it can't be told (another system, or the lookup failed).
	// Callers fall back to what they knew before: the port accepts
	// connections.
	OwnerUnknown Owner = iota
	// OwnerNone: nothing listens on the port.
	OwnerNone
	// OwnerSelf: the process asked about, or one it started, holds the
	// listening socket.
	OwnerSelf
	// OwnerOther: another process holds it.
	OwnerOther
)

func (o Owner) String() string {
	return [...]string{"unknown", "none", "self", "other"}[o]
}

// PortOwner says whether the socket that listens on the internal port belongs
// to the copy at pid: held by pid itself or by a process descended from it (an
// R that started a worker, a wrapper that started R). A copy counts as ready
// only when it does, so a stranger that took the port first, another user's
// shinyctl-api picking the same internal port, is never taken for the copy.
// The lookup is by the system's own tables: /proc on Linux, lsof on macOS
// (there is no way to ask the kernel for it without cgo that is stable across
// macOS releases, and lsof is part of every macOS); elsewhere it can't be told
// and says so.
func PortOwner(pid, port int) Owner { return portOwner(pid, port) }

// listenInodes returns the socket inodes of the LISTEN rows of a
// /proc/net/tcp or /proc/net/tcp6 table for port.
func listenInodes(table string, port int) []string {
	var inodes []string
	sc := bufio.NewScanner(strings.NewReader(table))
	sc.Scan() // the header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 || f[3] != "0A" { // 0A is TCP_LISTEN
			continue
		}
		_, p, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(p, 16, 32); err != nil || int(n) != port {
			continue
		}
		inodes = append(inodes, f[9])
	}
	return inodes
}

// descendants returns pid and every process descended from it, given each
// process's parent.
func descendants(pid int, parent map[int]int) []int {
	children := map[int][]int{}
	for p, pp := range parent {
		children[pp] = append(children[pp], p)
	}
	out := []int{pid}
	for i := 0; i < len(out); i++ {
		out = append(out, children[out[i]]...)
	}
	return out
}
