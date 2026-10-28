package process

import (
	"net"
	"os/exec"
	"reflect"
	"sort"
	"testing"
	"time"
)

const tcpSample = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:4E20 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41001 1 0000000000000000 100 0 0 10 0
   1: 0100007F:4E21 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41002 1 0000000000000000 100 0 0 10 0
   2: 0100007F:C350 0100007F:4E20 01 00000000:00000000 00:00000000 00000000  1000        0 41003 1 0000000000000000 100 0 0 10 0
   3: 00000000:4E20 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 41004 1 0000000000000000 100 0 0 10 0
   4: 0100007F:4E20 0100007F:C351 01 00000000:00000000 00:00000000 00000000  1000        0 41005 1 0000000000000000 100 0 0 10 0
`

const tcp6Sample = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:4E20 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41010 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41011 1 0000000000000000 100 0 0 10 0
`

func TestListenInodes(t *testing.T) {
	// 0x4E20 is 20000. Only LISTEN rows count: the established connection
	// (state 01) to the same port does not.
	if got := listenInodes(tcpSample, 20000); !reflect.DeepEqual(got, []string{"41001", "41004"}) {
		t.Errorf("tcp, port 20000: %v, want [41001 41004]", got)
	}
	if got := listenInodes(tcpSample, 20001); !reflect.DeepEqual(got, []string{"41002"}) {
		t.Errorf("tcp, port 20001: %v", got)
	}
	if got := listenInodes(tcp6Sample, 20000); !reflect.DeepEqual(got, []string{"41010"}) {
		t.Errorf("tcp6, port 20000: %v", got)
	}
	if got := listenInodes(tcp6Sample, 8080); !reflect.DeepEqual(got, []string{"41011"}) {
		t.Errorf("tcp6, port 8080: %v", got)
	}
	if got := listenInodes(tcpSample, 20999); len(got) != 0 {
		t.Errorf("a port nobody listens on: %v", got)
	}
	if got := listenInodes("", 20000); len(got) != 0 {
		t.Errorf("an empty table: %v", got)
	}
}

func TestDescendants(t *testing.T) {
	parent := map[int]int{10: 1, 11: 10, 12: 11, 13: 10, 20: 1, 21: 20}
	got := descendants(10, parent)
	sort.Ints(got)
	if !reflect.DeepEqual(got, []int{10, 11, 12, 13}) {
		t.Errorf("descendants of 10 = %v, want [10 11 12 13]", got)
	}
	if got := descendants(99, parent); !reflect.DeepEqual(got, []int{99}) {
		t.Errorf("descendants of a process nobody knows = %v", got)
	}
}

// The real tables of this machine: a listener of this process is its own; the
// same port asked about another process is another's; a port nobody listens on
// has no owner.
func TestPortOwner_RealSockets(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if got := PortOwner(selfPID(), port); got == OwnerUnknown {
		t.Skip("this system can't tell who owns a port")
	} else if got != OwnerSelf {
		t.Errorf("PortOwner(self, %d) = %v, want self", port, got)
	}

	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { other.Process.Kill(); other.Wait() }()
	if got := PortOwner(other.Process.Pid, port); got != OwnerOther {
		t.Errorf("PortOwner(another process, %d) = %v, want other", port, got)
	}

	ln2, _ := net.Listen("tcp4", "127.0.0.1:0")
	free := ln2.Addr().(*net.TCPAddr).Port
	ln2.Close()
	time.Sleep(50 * time.Millisecond)
	if got := PortOwner(selfPID(), free); got != OwnerNone {
		t.Errorf("PortOwner(self, %d) with nobody listening = %v, want none", free, got)
	}
}

// A listener of a child of the copy is the copy's: an R that started a worker.
func TestPortOwner_ADescendantsSocketIsTheCopys(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	// sh starts python as its child; python listens.
	child := exec.Command("sh", "-c", `python3 -c "import socket,time;s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('127.0.0.1',`+itoa(port)+`));s.listen(1);time.sleep(30)"; true`)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { child.Process.Kill(); child.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	var got Owner
	for time.Now().Before(deadline) {
		if got = PortOwner(child.Process.Pid, port); got == OwnerSelf || got == OwnerUnknown {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got == OwnerUnknown {
		t.Skip("this system can't tell who owns a port")
	}
	if got != OwnerSelf {
		t.Errorf("PortOwner(the shell, %d) = %v, want self: the listener is its child's", port, got)
	}
}
