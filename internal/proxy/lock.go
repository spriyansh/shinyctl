package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spriyansh/shinyctl/internal/process"
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

// takeoverWait is how long a load balancer started next to another waits for
// the other to be gone and its lock free.
var takeoverWait = 30 * time.Second

// waitLock takes the lock at path once its holder lets go, or gives up after
// timeout. With replacing set, it also gives up at once when the lock is
// taken by some other process than replacing: another load balancer replaced
// that one first, and this one must not go on serving next to it.
func waitLock(path string, info lockInfo, timeout time.Duration, replacing int) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := acquireLock(path, info)
		if err == nil {
			return f, nil
		}
		if replacing > 0 {
			if h := readHolderPID(path); h > 0 && h != replacing && h != info.PID {
				return nil, fmt.Errorf("another load balancer (pid %d) took over first", h)
			}
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readHolderPID(path string) int {
	info, _ := readLock(path)
	return info.PID
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

// commandLineFor is the command line of the load balancer whose config is
// configPath, as a substring: how a process is told to be shinyctl's load
// balancer before anything signals it.
func commandLineFor(configPath string) string {
	return "proxy --config " + configPath
}

// RunningPID is the process holding the lock beside configPath when it is
// demonstrably the load balancer for that config (its command line says so);
// otherwise 0. It reads the lock and nothing else, so it says the same
// whether or not a Supervisor is running, and it never starts anything.
func RunningPID(configPath string) int {
	lock := LockPath(configPath)
	if !lockHeld(lock) {
		return 0
	}
	info := waitHolder(lock)
	if info.PID > 0 && process.HasCommandLine(info.PID, commandLineFor(configPath)) {
		return info.PID
	}
	return 0
}

// StopRunning stops the load balancer for s through its lock, and says
// whether it signalled one. It does not depend on what a Supervisor
// remembers: that is empty when ensure failed or the load balancer is in
// restart backoff. A lock held by a process that is not demonstrably the
// load balancer is never signalled.
func StopRunning(s Settings) (stopped bool, err error) {
	admin := Admin{Socket: s.AdminSocket}
	command := commandLineFor(s.ConfigPath)
	return stopRunning(
		func() int { return RunningPID(s.ConfigPath) },
		func() int {
			pid, err := admin.ServingPID()
			if err != nil || pid <= 0 || !process.HasCommandLine(pid, command) {
				return 0
			}
			return pid
		},
		func(pid int) (bool, error) {
			return process.StopIf(pid, func() bool { return process.HasCommandLine(pid, command) })
		},
	)
}

// stopRunning is StopRunning with its three lookups as parameters. Round one
// stops the process that answers on the control socket when it is not the
// lock holder (a successor started with --after that still waits for the
// lock and would take it and go on serving once the holder is gone), then the
// holder. Round two stops a holder that has appeared meanwhile. There is no
// third round.
func stopRunning(holder, serving func() int, stop func(pid int) (bool, error)) (stopped bool, err error) {
	done := map[int]bool{}
	stopOne := func(pid int) {
		if pid <= 0 || done[pid] {
			return
		}
		done[pid] = true
		signaled, serr := stop(pid)
		stopped = stopped || signaled
		if serr != nil && err == nil {
			err = serr
		}
	}

	h := holder()
	if sv := serving(); sv != h {
		stopOne(sv)
	}
	stopOne(h)

	stopOne(holder())
	return stopped, err
}
