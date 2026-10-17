package process

import (
	"os"
	"path/filepath"
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
