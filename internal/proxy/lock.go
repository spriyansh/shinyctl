package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// LockPath is the proxy.lock beside configPath. The proxy holds an
// exclusive flock on it for as long as it runs, so the lock is also the
// answer to "is a load balancer running": the kernel drops it on any exit,
// kill -9 included, so it can't go stale the way a pid file does.
//
// It is what keeps a second proxy out. The engine unlinks a stale control
// socket by itself and shares listening ports through SO_REUSEPORT, so
// without the lock a second instance would start, take the socket over and
// split the traffic. Deleting ~/.shinyctl/proxy under a running proxy
// defeats it, and isn't supported.
func LockPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "proxy.lock")
}

// lockInfo is what the proxy writes into the lock file once it holds it:
// who it is, and which shinyctl build started it. shinyctl-api reads it to
// find the process and to tell whether it is running the build it came
// from.
type lockInfo struct {
	PID   int    `json:"pid"`
	Build string `json:"build"`
}

// acquireLock takes the exclusive lock on path without waiting and records
// info in it. The returned file must stay open until the process exits.
// When another process holds the lock the error names it.
func acquireLock(path string, info lockInfo) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err != syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("locking %s: %w", path, err)
		}
		return nil, fmt.Errorf("proxy: already running (pid %d); not starting a second one", waitHolder(path).PID)
	}

	data, err := json.Marshal(info)
	if err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.WriteAt(data, 0); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// waitHolder reads the record of the process holding the lock at path. The
// holder writes it right after taking the lock, so a read in that gap sees an
// empty file; wait for it briefly. Zero when there is none.
func waitHolder(path string) lockInfo {
	for i := 0; i < 20; i++ {
		if info, err := readLock(path); err == nil && info.PID > 0 {
			return info
		}
		time.Sleep(10 * time.Millisecond)
	}
	return lockInfo{}
}

// lockHeld reports whether a process holds the lock at path. A missing
// file is not held. It takes the lock itself to find out, and lets go at
// once.
func lockHeld(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err == syscall.EWOULDBLOCK
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// readLock reads the lock file's record.
func readLock(path string) (lockInfo, error) {
	var info lockInfo
	data, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	err = json.Unmarshal(data, &info)
	return info, err
}

// BuildID identifies the executable this process runs from: its size and
// modification time. A proxy records it in its lock, and shinyctl-api
// replaces a proxy whose build differs from its own, so an upgraded
// shinyctl doesn't leave the previous release's load balancer running.
func BuildID() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%x-%x", fi.Size(), fi.ModTime().UnixNano())
}
