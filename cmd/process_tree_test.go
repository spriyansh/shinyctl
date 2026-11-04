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
