package process

import (
	"os"
	"strconv"
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

	table, err := tableIn(root)
	if err != nil {
		return OwnerUnknown
	}
	parent := parents(table)
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
