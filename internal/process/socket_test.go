package process

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// tempSocketPath returns a path short enough to actually bind a Unix
// socket on. t.TempDir() bakes the full (sub)test name into the path,
// which easily exceeds sockaddr_un.sun_path's limit — 104 bytes on macOS,
// 108 on Linux — even though it's a perfectly ordinary filesystem path
// otherwise. os.MkdirTemp with a short, fixed prefix avoids that.
func tempSocketPath(t *testing.T) string {
	dir, err := os.MkdirTemp("", "sh")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	return filepath.Join(dir, "test.sock")
}

func TestListenUnix(t *testing.T) {
	t.Run("fresh path", func(t *testing.T) {
		path := tempSocketPath(t)

		ln, err := ListenUnix(path)
		if err != nil {
			t.Fatalf("ListenUnix: %v", err)
		}
		defer ln.Close()
	})

	t.Run("stale socket file is cleaned up", func(t *testing.T) {
		path := tempSocketPath(t)

		if err := os.WriteFile(path, []byte("leftover from a crash"), 0600); err != nil {
			t.Fatalf("seeding stale file: %v", err)
		}

		ln, err := ListenUnix(path)
		if err != nil {
			t.Fatalf("ListenUnix: %v", err)
		}
		defer ln.Close()
	})

	t.Run("refuses to start a second instance", func(t *testing.T) {
		path := tempSocketPath(t)

		first, err := ListenUnix(path)
		if err != nil {
			t.Fatalf("first ListenUnix: %v", err)
		}
		defer first.Close()

		if _, err := ListenUnix(path); err == nil {
			t.Error("second ListenUnix succeeded, want error (already running)")
		}
	})
}

func TestCheckSocketPath(t *testing.T) {
	ok := strings.Repeat("a", MaxSocketPath)
	if err := CheckSocketPath(ok); err != nil {
		t.Errorf("a %d-byte path: %v, want nil", len(ok), err)
	}

	long := strings.Repeat("a", MaxSocketPath+1)
	want := long + " is too long for a socket (104 bytes, at most 103) — use a shorter home directory"
	if err := CheckSocketPath(long); err == nil || err.Error() != want {
		t.Errorf("a 104-byte path: %v, want %q", err, want)
	}
}

// TestCheckSocketPaths: api.sock and proxy/admin.sock both have to fit, and
// the message names the one that doesn't.
func TestCheckSocketPaths(t *testing.T) {
	base, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	// A home of n bytes: api.sock is n+19 bytes, proxy/admin.sock n+27.
	home := func(n int) string {
		return filepath.Join(base, strings.Repeat("x", n-len(base)-1))
	}
	if len(base) > 60 {
		t.Skipf("temp dir %q is too long to build the test homes", base)
	}

	t.Run("both fit", func(t *testing.T) {
		t.Setenv("HOME", home(MaxSocketPath-27))
		if err := CheckSocketPaths(); err != nil {
			t.Errorf("CheckSocketPaths = %v, want nil", err)
		}
	})
	t.Run("only admin.sock is too long", func(t *testing.T) {
		t.Setenv("HOME", home(MaxSocketPath-26))
		err := CheckSocketPaths()
		if err == nil || !strings.Contains(err.Error(), "admin.sock is too long for a socket") {
			t.Errorf("CheckSocketPaths = %v, want admin.sock named", err)
		}
	})
	t.Run("api.sock is too long", func(t *testing.T) {
		t.Setenv("HOME", home(MaxSocketPath-18))
		err := CheckSocketPaths()
		if err == nil || !strings.Contains(err.Error(), "api.sock is too long for a socket") {
			t.Errorf("CheckSocketPaths = %v, want api.sock named", err)
		}
	})
}

// TestEnsureAPIRunning_LongHomeSaysWhy: a home directory too long for the
// sockets is reported by name, not as a daemon that "did not come up".
func TestEnsureAPIRunning_LongHomeSaysWhy(t *testing.T) {
	base, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if len(base) > 60 {
		t.Skipf("temp dir %q is too long to build the test home", base)
	}
	t.Setenv("HOME", filepath.Join(base, strings.Repeat("x", MaxSocketPath-26-len(base)-1)))

	err = EnsureAPIRunning()
	if err == nil || !strings.Contains(err.Error(), "admin.sock is too long for a socket") {
		t.Errorf("EnsureAPIRunning = %v, want admin.sock named", err)
	}
}

// Many starting at once: exactly one listens, and the others are told that one
// is running. Without the lock one could remove the socket another had just
// bound, and two would end up running, one of them without a socket.
func TestListenUnix_OnlyOneOfManyAtOnceListens(t *testing.T) {
	dir, err := os.MkdirTemp("", "lu")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	for round := 0; round < 30; round++ {
		path := filepath.Join(dir, "api.sock")
		const n = 8
		var wg sync.WaitGroup
		var mu sync.Mutex
		var listeners []net.Listener
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if ln, err := ListenUnix(path); err == nil {
					mu.Lock()
					listeners = append(listeners, ln)
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()
		if len(listeners) != 1 {
			t.Fatalf("round %d: %d of %d listened, want exactly one", round, len(listeners), n)
		}
		if !SocketDialable(path) {
			t.Fatalf("round %d: the one that listens has no socket", round)
		}
		listeners[0].Close()
		os.Remove(path)
	}
}

// A holder of the start-up lock that does not let go makes ListenUnix give up
// after socketLockWait with a plain message, and nothing is left behind; once
// the holder lets go it works.
func TestListenUnix_GivesUpWhenTheStartUpLockIsNeverFreed(t *testing.T) {
	dir, err := os.MkdirTemp("", "lu")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	holder, err := os.OpenFile(filepath.Join(dir, "api.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	orig := socketLockWait
	socketLockWait = 300 * time.Millisecond
	defer func() { socketLockWait = orig }()

	path := filepath.Join(dir, "api.sock")
	start := time.Now()
	ln, err := ListenUnix(path)
	if err == nil {
		ln.Close()
		t.Fatal("ListenUnix succeeded while the start-up lock was held")
	}
	if took := time.Since(start); took < 250*time.Millisecond || took > 3*time.Second {
		t.Errorf("ListenUnix took %s to give up, want about 300ms", took)
	}
	if !strings.Contains(err.Error(), "another shinyctl-api is starting") {
		t.Errorf("error = %q, want it to say another shinyctl-api is starting", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("a socket was left behind")
	}

	syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	ln, err = ListenUnix(path)
	if err != nil {
		t.Fatalf("ListenUnix after the holder let go: %v", err)
	}
	ln.Close()
}

// A start-up lock that is let go of after a moment is waited for: the second
// start gets it, and is not refused.
func TestListenUnix_WaitsForAStartUpLockThatIsLetGo(t *testing.T) {
	dir, err := os.MkdirTemp("", "lu")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	holder, err := os.OpenFile(filepath.Join(dir, "api.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	fd := int(holder.Fd())
	time.AfterFunc(500*time.Millisecond, func() { syscall.Flock(fd, syscall.LOCK_UN) })

	start := time.Now()
	ln, err := ListenUnix(filepath.Join(dir, "api.sock"))
	if err != nil {
		t.Fatalf("ListenUnix with a lock freed after 0.5 s: %v", err)
	}
	ln.Close()
	if took := time.Since(start); took < 400*time.Millisecond {
		t.Errorf("ListenUnix took %s, want it to have waited for the holder", took)
	}
}
