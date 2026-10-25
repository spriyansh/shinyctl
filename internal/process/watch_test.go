package process

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

// orphan starts `sleep` so that it is not a child of the test: the shell
// that started it exits, and init adopts the sleep. It returns the sleep's
// PID and a record for it.
func orphan(t *testing.T, seconds string) state.AppState {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep "+seconds+" & echo $!")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	cmd.Wait()
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("pid %q: %v", line, err)
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	return state.AppState{PID: pid, StartedAt: time.Now(), EntrypointFile: "sleep " + seconds}
}

func watchOrSkip(t *testing.T, app state.AppState) <-chan struct{} {
	t.Helper()
	ch, err := WatchExit(manifest.AppSpec{}, app)
	if errors.Is(err, ErrWatchUnsupported) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EPERM) {
		t.Skipf("this system can't watch a process: %v", err)
	}
	if err != nil {
		t.Fatalf("WatchExit: %v", err)
	}
	return ch
}

func TestWatchExit_ProcessThatIsNotOurChild(t *testing.T) {
	app := orphan(t, "30")
	ch := watchOrSkip(t, app)
	select {
	case <-ch:
		t.Fatal("the channel closed while the process was running")
	case <-time.After(200 * time.Millisecond):
	}
	killed := time.Now()
	if err := syscall.Kill(app.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
		t.Logf("closed after %v", time.Since(killed))
	case <-time.After(time.Second):
		t.Fatal("the channel did not close within a second of the exit")
	}
}

func TestWatchExit_ChildToo(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	app := state.AppState{PID: cmd.Process.Pid, StartedAt: time.Now(), EntrypointFile: "sleep 30"}
	ch := watchOrSkip(t, app)
	cmd.Process.Kill()
	go cmd.Wait()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("the channel did not close within a second of the exit")
	}
}

func TestWatchExit_GoneIsAnErrorAtOnce(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	app := state.AppState{PID: cmd.Process.Pid, StartedAt: time.Now(), EntrypointFile: "true"}
	if _, err := WatchExit(manifest.AppSpec{}, app); err == nil {
		t.Fatal("WatchExit of a gone process = nil error")
	} else if errors.Is(err, ErrWatchUnsupported) || errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EPERM) {
		t.Skipf("this system can't watch a process: %v", err)
	} else if !errors.Is(err, ErrNotOurs) {
		t.Errorf("error = %v, want ErrNotOurs", err)
	}
	if _, err := WatchExit(manifest.AppSpec{}, state.AppState{}); !errors.Is(err, ErrNotOurs) {
		t.Errorf("no pid: error = %v, want ErrNotOurs", err)
	}
}

func TestWatchExit_NotOursWhenTheRecordDoesNotMatch(t *testing.T) {
	app := orphan(t, "30")
	// Make sure the system can watch at all, so a skip is not hidden here.
	watchOrSkip(t, app)

	other := app
	other.EntrypointFile = "an-entirely-different-app.R"
	if _, err := WatchExit(manifest.AppSpec{}, other); !errors.Is(err, ErrNotOurs) {
		t.Errorf("a process that is another app's: error = %v, want ErrNotOurs", err)
	}
	other = app
	other.Port = 4321 // the command line doesn't have port=4321,
	if _, err := WatchExit(manifest.AppSpec{}, other); !errors.Is(err, ErrNotOurs) {
		t.Errorf("a process on another port: error = %v, want ErrNotOurs", err)
	}
	if err := syscall.Kill(app.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
}

func TestWatchExit_ReleasesItsDescriptors(t *testing.T) {
	before := openFDs(t)
	for i := 0; i < 20; i++ {
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		app := state.AppState{PID: cmd.Process.Pid, StartedAt: time.Now(), EntrypointFile: "sleep 30"}
		ch := watchOrSkip(t, app)
		// A mismatch, while the process is there, closes its handle too.
		other := app
		other.EntrypointFile = "nothing"
		WatchExit(manifest.AppSpec{}, other)
		cmd.Process.Kill()
		cmd.Wait()
		<-ch
	}
	time.Sleep(100 * time.Millisecond)
	if after := openFDs(t); after > before+2 {
		t.Errorf("%d descriptors open, %d before: the handles are not closed", after, before)
	}
}

// openFDs counts this process's open descriptors by probing.
func openFDs(t *testing.T) int {
	t.Helper()
	n := 0
	for fd := 3; fd < 1024; fd++ {
		if _, err := fcntlGetFD(fd); err == nil {
			n++
		}
	}
	return n
}

// Signals to the process that watches (SIGCHLD from its own children, the
// runtime's preemption signals) must not end a wait early: the wait is
// retried when the system call is interrupted.
func TestWatchExit_InterruptedWaitGoesOn(t *testing.T) {
	var chans []<-chan struct{}
	for i := 0; i < 16; i++ {
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		chans = append(chans, watchOrSkip(t, state.AppState{PID: cmd.Process.Pid, StartedAt: time.Now(), EntrypointFile: "sleep 30"}))
	}
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 600; i++ {
		syscall.Kill(os.Getpid(), syscall.SIGCHLD)
		syscall.Kill(os.Getpid(), syscall.SIGURG)
		time.Sleep(2 * time.Millisecond)
	}
	for i, ch := range chans {
		select {
		case <-ch:
			t.Errorf("wait %d of %d ended while the process still ran", i+1, len(chans))
		default:
		}
	}
}
