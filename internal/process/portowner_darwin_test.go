package process

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// As an ordinary user lsof lists this user's processes only: another user's
// listener is invisible to it. The owner must not read that as "nobody":
// connect first, and a port that accepts with no listener in the copy's tree
// is another program's. There is no second account to make a real foreign
// listener with, so lsof is stood in for by what it does for another user's
// socket: nothing, exit 1.
func TestPortOwnerDarwin_ALeaderInvisibleToLsofIsAnotherProgramsWhenItAccepts(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	exit1 := func() error { return exec.Command("false").Run() }() // an *exec.ExitError with code 1
	old := listenersRun
	defer func() { listenersRun = old }()
	listenersRun = func(name string, args ...string) (string, error) {
		if name == "ps" {
			return strconv.Itoa(os.Getpid()) + " 1\n", nil
		}
		return "", exit1 // lsof finds no listener of ours
	}
	if got := portOwner(os.Getpid(), port); got != OwnerOther {
		t.Errorf("a port that accepts with no listener of ours visible = %v, want other", got)
	}

	// Nobody accepts: no owner yet, whatever lsof says.
	ln2, _ := net.Listen("tcp4", "127.0.0.1:0")
	closed := ln2.Addr().(*net.TCPAddr).Port
	ln2.Close()
	if got := portOwner(os.Getpid(), closed); got != OwnerNone {
		t.Errorf("a port that does not accept = %v, want none", got)
	}

	// lsof sees the copy's listener: ours.
	listenersRun = func(name string, args ...string) (string, error) {
		if name == "ps" {
			return strconv.Itoa(os.Getpid()) + " 1\n", nil
		}
		return "p" + strconv.Itoa(os.Getpid()) + "\n", nil
	}
	if got := portOwner(os.Getpid(), port); got != OwnerSelf {
		t.Errorf("lsof finds the copy's listener = %v, want self", got)
	}

	// lsof broken: can't tell.
	listenersRun = func(name string, args ...string) (string, error) {
		if name == "ps" {
			return strconv.Itoa(os.Getpid()) + " 1\n", nil
		}
		return "", errors.New("no such file")
	}
	if got := portOwner(os.Getpid(), port); got != OwnerUnknown {
		t.Errorf("lsof failing = %v, want unknown", got)
	}
}

// The connect comes before any lookup: a copy that binds while the lookup runs
// is then seen as "nobody yet" and asked again later, not taken for a stranger
// (a lookup that began before the bind finds nothing, and a connect after it
// would accept).
func TestPortOwnerDarwin_ConnectsBeforeLookingUp(t *testing.T) {
	probe, _ := net.Listen("tcp4", "127.0.0.1:0")
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	var ln net.Listener
	defer func() {
		if ln != nil {
			ln.Close()
		}
	}()
	exit1 := exec.Command("false").Run()
	old := listenersRun
	defer func() { listenersRun = old }()
	var looked int
	listenersRun = func(name string, args ...string) (string, error) {
		looked++
		var err error
		ln, err = net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port)) // the copy binds during the lookup
		if err != nil {
			t.Error(err)
		}
		if name == "ps" {
			return strconv.Itoa(os.Getpid()) + " 1\n", nil
		}
		return "", exit1
	}
	if got := portOwner(os.Getpid(), port); got != OwnerNone || looked != 0 {
		t.Errorf("portOwner = %v after %d lookups, want none with no lookup: nothing accepted when it was asked", got, looked)
	}
}
