package process

import (
	"os"
	"strconv"
	"strings"
)

func portOwner(pid, port int) Owner { return portOwnerIn(procDir, pid, port) }

// readFDs lists a process's descriptors; a variable for the tests.
var readFDs = os.ReadDir

// portOwnerIn is portOwner reading the process table at root (a variable of
// the tests' own, like procDir).
func portOwnerIn(root string, pid, port int) Owner {
	var inodes []string
	for _, name := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(root + "/net/" + name)
		if err != nil {
			if name == "tcp" {
				return OwnerUnknown
			}
			continue
		}
		inodes = append(inodes, listenInodes(string(data), port)...)
	}
	if len(inodes) == 0 {
		return OwnerNone
	}
	want := map[string]bool{}
	for _, in := range inodes {
		want["socket:["+in+"]"] = true
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return OwnerUnknown
	}
	parent := map[int]int{}
	for _, e := range entries {
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if pp, ok := parentOf(root, n); ok {
			parent[n] = pp
		}
	}
	unreadable := false
	for _, p := range descendants(pid, parent) {
		fds, err := readFDs(root + "/" + strconv.Itoa(p) + "/fd")
		if err != nil {
			// A process of the copy's own tree that won't show its
			// descriptors (non-dumpable, file capabilities, a setuid
			// binary) can't be ruled out: the answer is then not "another
			// program" but "can't tell".
			if os.IsPermission(err) {
				unreadable = true
			}
			continue
		}
		for _, fd := range fds {
			if link, err := os.Readlink(root + "/" + strconv.Itoa(p) + "/fd/" + fd.Name()); err == nil && want[link] {
				return OwnerSelf
			}
		}
	}
	if unreadable {
		return OwnerUnknown
	}
	return OwnerOther
}

// parentOf reads the parent of process n from its stat line: the fields after
// the command name, which is in parentheses and may hold spaces.
func parentOf(root string, n int) (int, bool) {
	data, err := os.ReadFile(root + "/" + strconv.Itoa(n) + "/stat")
	if err != nil {
		return 0, false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, false
	}
	pp, err := strconv.Atoi(f[1])
	return pp, err == nil
}
