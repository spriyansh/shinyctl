package process

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A made-up /proc: process 100 holds socket 41001 (listening on 20000), its
// child 101 holds 41010 (tcp6, 20001), and process 200 holds 41004, which
// also listens on 20000.
func fakeProcWithSockets(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, data string) {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("net/tcp", tcpSample)
	write("net/tcp6", `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:4E21 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41010 1 0000000000000000 100 0 0 10 0
`)
	proc := func(pid, ppid int, name string, socks ...string) {
		write(itoa(pid)+"/stat", itoa(pid)+" ("+name+") S "+itoa(ppid)+" 1 1 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 1 0 100 1000 100 18446744073709551615\n")
		if err := os.MkdirAll(filepath.Join(root, itoa(pid), "fd"), 0o755); err != nil {
			t.Fatal(err)
		}
		for i, s := range socks {
			if err := os.Symlink("socket:["+s+"]", filepath.Join(root, itoa(pid), "fd", itoa(3+i))); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("/dev/null", filepath.Join(root, itoa(pid), "fd", "0")); err != nil {
			t.Fatal(err)
		}
	}
	proc(100, 1, "R", "41001")
	proc(101, 100, "a (worker) with spaces", "41010")
	proc(200, 1, "other", "41004")
	return root
}

func TestPortOwnerIn_FakeProc(t *testing.T) {
	root := fakeProcWithSockets(t)
	for _, tc := range []struct {
		name      string
		pid, port int
		want      Owner
	}{
		{"the copy holds the socket", 100, 20000, OwnerSelf},
		{"its child holds a tcp6 socket on another port", 100, 20001, OwnerSelf},
		{"a stranger holds the other socket on 20000, and the copy holds one too", 200, 20000, OwnerSelf},
		{"the copy asked about a port only a stranger's socket serves", 100, 20000, OwnerSelf},
		{"nobody listens", 100, 20500, OwnerNone},
		{"the stranger asked about the copy's tcp6 port", 200, 20001, OwnerOther},
	} {
		if got := portOwnerIn(root, tc.pid, tc.port); got != tc.want {
			t.Errorf("%s: portOwnerIn(%d, %d) = %v, want %v", tc.name, tc.pid, tc.port, got, tc.want)
		}
	}
}

func TestPortOwnerIn_AStrangerHoldsThePort(t *testing.T) {
	root := fakeProcWithSockets(t)
	// Drop the copy's own socket from the table: only 41004 (process 200)
	// listens on 20000 now.
	os.WriteFile(filepath.Join(root, "net/tcp"), []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   3: 00000000:4E20 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 41004 1 0000000000000000 100 0 0 10 0
`), 0o644)
	if got := portOwnerIn(root, 100, 20000); got != OwnerOther {
		t.Errorf("the copy's own socket is not listening, a stranger's is: %v, want other", got)
	}
	if got := portOwnerIn(root, 200, 20000); got != OwnerSelf {
		t.Errorf("the stranger asked about itself: %v, want self", got)
	}
}

func TestPortOwnerIn_NoProcTable(t *testing.T) {
	if got := portOwnerIn(t.TempDir(), 1, 20000); got != OwnerUnknown {
		t.Errorf("a /proc without net/tcp: %v, want unknown", got)
	}
}

// A copy whose descriptors can't be read (it made itself non-dumpable, or its
// binary has file capabilities) is not "another program": nothing is known.
func TestPortOwnerIn_UnreadableDescriptorsAreUnknownNotOther(t *testing.T) {
	root := fakeProcWithSockets(t)
	old := readFDs
	defer func() { readFDs = old }()
	readFDs = func(name string) ([]os.DirEntry, error) {
		if filepath.Base(filepath.Dir(name)) == "100" {
			return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EACCES}
		}
		return os.ReadDir(name)
	}
	// 100's own socket can't be seen, and nothing else of its tree holds the port.
	os.WriteFile(filepath.Join(root, "net/tcp"), []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   3: 00000000:4E20 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 41001 1 0000000000000000 100 0 0 10 0
`), 0o644)
	if got := portOwnerIn(root, 100, 20000); got != OwnerUnknown {
		t.Errorf("unreadable descriptors of the copy = %v, want unknown", got)
	}
	// A process that is simply gone (no descriptors to read) is not "can't tell":
	// the copy's socket isn't there, and the port is another's.
	readFDs = func(name string) ([]os.DirEntry, error) {
		if filepath.Base(filepath.Dir(name)) == "100" {
			return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.ENOENT}
		}
		return os.ReadDir(name)
	}
	if got := portOwnerIn(root, 100, 20000); got != OwnerOther {
		t.Errorf("a vanished process = %v, want other", got)
	}
	// Readable, and the socket is a stranger's: still other.
	readFDs = os.ReadDir
	if got := portOwnerIn(root, 200, 20000); got != OwnerOther {
		t.Errorf("readable tree, the socket is another process's = %v, want other", got)
	}
}
