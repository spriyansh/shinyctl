package process

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
