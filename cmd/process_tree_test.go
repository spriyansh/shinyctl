/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/state"
)

// workersSpec is an app of one copy whose fake Rscript starts two workers and
// a third that calls setsid, as a callr worker does.
func workersSpec(t *testing.T, name string) manifest.AppSpec {
	t.Helper()
	spec := copiesSpec(t, name, 1, 0)
	spec.Env = map[string]string{"FAKE_RSCRIPT_WORKERS": "2", "FAKE_RSCRIPT_DETACHED": "1"}
	return spec
}

// workersOf asks the copy for the pids of its workers: the ones in its group
// and the detached one.
func workersOf(t *testing.T, rec state.AppState) (inGroup []int, detached int) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + dialOf(rec) + "/workers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, field := range strings.Fields(string(body)) {
		name, list, _ := strings.Cut(field, "=")
		for _, p := range strings.Split(list, ",") {
			pid, err := strconv.Atoi(p)
			if err != nil || pid == 0 {
				continue
			}
			if name == "detached" {
				detached = pid
			} else {
				inGroup = append(inGroup, pid)
			}
		}
	}
	if len(inGroup) != 2 || detached == 0 {
		t.Fatalf("/workers = %q, want two workers and a detached one", body)
	}
	return inGroup, detached
}

// waitGoneAll waits until none of pids is running.
func waitGoneAll(t *testing.T, what string, pids ...int) {
	t.Helper()
	waitUntil(t, what, 10*time.Second, func() bool {
		for _, pid := range pids {
			if syscall.Kill(pid, 0) != syscall.ESRCH {
				return false
			}
		}
		return true
	})
}

// The fake Rscript leads a group of its own, and its workers are in it, apart
// from the one that left.
func TestWorkers_AreInTheCopysGroup(t *testing.T) {
	useFakeRscript(t)
	spec := workersSpec(t, "app")
	cleanupApp(t, spec)
	rec := applyOK(t, spec)
	waitReady(t, process.InternalHost, rec.Port)
	workers, detached := workersOf(t, rec)
	t.Cleanup(func() {
		for _, pid := range append(workers, detached) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	for _, pid := range workers {
		if pgid, _ := syscall.Getpgid(pid); pgid != rec.PID {
			t.Errorf("worker %d is in group %d, want the copy's %d", pid, pgid, rec.PID)
		}
	}
	if pgid, _ := syscall.Getpgid(detached); pgid == rec.PID {
		t.Errorf("the detached worker is in the copy's group")
	}
}

// stop ends the copy's workers with it, the detached one included.
func TestStop_EndsWorkers(t *testing.T) {
	useFakeRscript(t)
	spec := workersSpec(t, "app")
	cleanupApp(t, spec)
	rec := applyOK(t, spec)
	waitReady(t, process.InternalHost, rec.Port)
	workers, detached := workersOf(t, rec)
	all := append([]int{rec.PID, detached}, workers...)
	t.Cleanup(func() {
		for _, pid := range all {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	if r := postJSON(t, handleStop, spec); r.Code != http.StatusOK {
		t.Fatalf("stop: status %d: %s", r.Code, r.Body.String())
	}
	waitGoneAll(t, "the copy and its workers to be gone after stop", all...)
}

// A copy that a replacement or a scale-down stops goes through stopCopies:
// its workers go too.
func TestReplace_EndsOldWorkers(t *testing.T) {
	useFakeRscript(t)
	settleNow(t)
	spec := workersSpec(t, "app")
	cleanupApp(t, spec)
	applyOK(t, spec)
	pool := waitRoles(t, spec, 1, 0)
	old := copyByDial(t, spec, pool.Active[0])
	workers, detached := workersOf(t, old)
	all := append([]int{old.PID, detached}, workers...)
	t.Cleanup(func() {
		for _, pid := range all {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	restartResponse(t, spec)
	waitPool(t, spec, "the switch", func(p proxy.Pool) bool {
		return len(p.Active) == 1 && p.Active[0] != pool.Active[0]
	})
	waitStopped(t, spec, old)
	waitGoneAll(t, "the old copy's workers to be gone", all...)
}

// crashingSpec is an app of one copy that never restarts, whose fake Rscript
// starts workers and then crashes after a while.
func crashingSpec(t *testing.T, after string) manifest.AppSpec {
	t.Helper()
	spec := workersSpec(t, "app")
	spec.RestartPolicy = manifest.RestartNever
	spec.Env["FAKE_RSCRIPT_EXIT_AFTER"] = after
	return spec
}

// killOnCleanup ends pids when the test does, whatever the test found.
func killOnCleanup(t *testing.T, pids ...int) {
	t.Helper()
	t.Cleanup(func() {
		for _, pid := range pids {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// A copy that crashes leaves its workers in its group: they are ended, and the
// log says so. The detached one is not found (its parent is gone) and runs on.
func TestSweep_AfterACrash(t *testing.T) {
	useFakeRscript(t)
	buf := captureLog(t)
	spec := crashingSpec(t, "2s")
	cleanupApp(t, spec)
	rec := applyOK(t, spec)
	waitReady(t, process.InternalHost, rec.Port)
	workers, detached := workersOf(t, rec)
	killOnCleanup(t, append(workers, detached)...)

	waitGoneAll(t, "the workers of the crashed copy to be swept", append(workers, rec.PID)...)
	if syscall.Kill(detached, 0) != nil {
		t.Errorf("the detached worker %d was ended; it is no longer anyone's descendant and runs on", detached)
	}
	waitUntil(t, "the sweep line", 5*time.Second, func() bool { return strings.Contains(buf.String(), "ended them") })
	want := "copy 1 (pid " + strconv.Itoa(rec.PID) + ") exited and left 2 processes; ended them"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("log = %q, want a line with %q", buf.String(), want)
	}
	if again := loadApp(t, spec); again.PID != rec.PID || again.CrashCount != 1 {
		t.Errorf("record = %+v, want the crash counted once and no restart (restart-policy never)", again)
	}
}

// The members are noted the moment the exit is seen, while recordExit still
// waits for the state lock: a cycle can hold it for seconds, and the note is
// what proves the group is the copy's.
func TestSweep_NoteBeforeTheLock(t *testing.T) {
	useFakeRscript(t)
	buf := captureLog(t) // the sweep's own line, which ends the test: no goroutine outlives it
	taken := make(chan int, 16)
	useHooks(t, hooks{NoteGroup: func(pgid int) (process.Note, bool) {
		n, ok := process.NoteGroup(pgid)
		select {
		case taken <- pgid:
		default:
		}
		return n, ok
	}})
	spec := crashingSpec(t, "2s")
	cleanupApp(t, spec)
	rec := applyOK(t, spec)
	waitReady(t, process.InternalHost, rec.Port)
	workers, detached := workersOf(t, rec)
	killOnCleanup(t, append(workers, detached)...)

	unlock := state.Lock(spec.AppStateFile)
	unlocked := false
	defer func() {
		if !unlocked {
			unlock()
		}
	}()
	deadline := time.After(15 * time.Second)
	for noted := false; !noted; {
		select {
		case pgid := <-taken:
			noted = pgid == rec.PID
		case <-deadline:
			t.Fatal("the group was not noted while the state lock was held")
		}
	}
	for _, pid := range workers {
		if syscall.Kill(pid, 0) != nil {
			t.Errorf("worker %d was ended before the exit was recorded", pid)
		}
	}
	unlock()
	unlocked = true
	waitGoneAll(t, "the workers to be swept under the note", workers...)
	waitUntil(t, "the sweep line", 5*time.Second, func() bool { return strings.Contains(buf.String(), "ended them") })
}

// A stop is not an exit the copy chose: its workers go with it, and the log has
// no sweep line.
func TestSweep_NotAfterStop(t *testing.T) {
	useFakeRscript(t)
	buf := captureLog(t)
	spec := workersSpec(t, "app")
	cleanupApp(t, spec)
	rec := applyOK(t, spec)
	waitReady(t, process.InternalHost, rec.Port)
	workers, detached := workersOf(t, rec)
	killOnCleanup(t, append(workers, detached)...)

	if r := postJSON(t, handleStop, spec); r.Code != http.StatusOK {
		t.Fatalf("stop: status %d: %s", r.Code, r.Body.String())
	}
	waitGoneAll(t, "the copy and its workers", append(workers, detached, rec.PID)...)
	time.Sleep(500 * time.Millisecond)
	if strings.Contains(buf.String(), "ended them") {
		t.Errorf("a stop was logged as a sweep:\n%s", buf.String())
	}
}

// A copy adopted from an earlier daemon is swept too, when its leader led its
// group and exits without a stop.
func TestSweep_AdoptedCopy(t *testing.T) {
	skipWithoutWatching(t)
	useFakeRscript(t)
	buf := captureLog(t)
	spec := workersSpec(t, "app")
	spec.RestartPolicy = manifest.RestartNever
	var a state.AppState
	t.Cleanup(func() {
		postJSON(t, handleDelete, spec)
		killIfAlive(spec, a)
	})
	a = startCopy(t, spec)
	waitServed(t, spec, a.PID)
	workers, detached := workersOf(t, a)
	killOnCleanup(t, append(workers, detached)...)
	reconcileApp(spec.AppStateFile) // sees it, and starts watching it
	if _, ok := adoptedWatched.Load(a.PID); !ok {
		t.Fatal("no watcher on the adopted copy")
	}

	syscall.Kill(a.PID, syscall.SIGKILL)
	waitGoneAll(t, "the adopted copy's workers to be swept", workers...)
	waitUntil(t, "the sweep line", 5*time.Second, func() bool { return strings.Contains(buf.String(), "ended them") })
}
