package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// footprintSample is footprint's default output for three processes: the
// blocks are sorted by size and not in the order asked, the second name holds
// spaces and brackets, a block lists what it shares with another process
// (naming that process's PID, which is asked for too), and the shared cache
// and the Summary, both framed by rules, close it.
const footprintSample = `======================================================================
Google Chrome Helper (Renderer) [713]: 64-bit    Footprint: 90000000 B (16384 bytes per page)
======================================================================

      Dirty         Clean   Reclaimable    Regions    Category
        ---           ---           ---        ---    ---
  60000000 B           0 B           0 B          5    MALLOC_SMALL
  30000000 B      573440 B           0 B        140    __DATA
        ---           ---           ---        ---    ---
  90000000 B      573440 B           0 B        411    TOTAL

Auxiliary data:
    phys_footprint: 91000000 B
    phys_footprint_peak: 99000000 B


======================================================================
R [x] [85910]: 64-bit    Footprint: 1655120 B (16384 bytes per page)
======================================================================

      Dirty         Clean   Reclaimable    Regions    Category
        ---           ---           ---        ---    ---
   442368 B           0 B           0 B          5    MALLOC_SMALL
   327680 B           0 B           0 B          8    MALLOC metadata
        ---           ---           ---        ---    ---
  1655120 B      573440 B           0 B        411    TOTAL

Shared with zsh [1104]:
      Dirty         Clean   Reclaimable    Regions    Category
        ---           ---           ---        ---    ---
    32768 B           0 B           0 B          1    untagged (VM_ALLOCATE)
        ---           ---           ---        ---    ---
    32768 B           0 B           0 B          1    TOTAL

Auxiliary data:
    phys_footprint: 1671504 B
    phys_footprint_peak: 1687888 B


======================================================================
zsh [1104]: 64-bit    Footprint: 500000 B (16384 bytes per page)
======================================================================

      Dirty         Clean   Reclaimable    Regions    Category
        ---           ---           ---        ---    ---
   500000 B           0 B           0 B          5    MALLOC_SMALL
        ---           ---           ---        ---    ---
   500000 B      573440 B           0 B         41    TOTAL

Shared with R [x] [85910]:
      Dirty         Clean   Reclaimable    Regions    Category
        ---           ---           ---        ---    ---
    32768 B           0 B           0 B          1    untagged (VM_ALLOCATE)
        ---           ---           ---        ---    ---
    32768 B           0 B           0 B          1    TOTAL

Auxiliary data:
    phys_footprint_peak: 600000 B

======================================================================
Shared Cache 157E6D2E-2E5C-39B1-8F2A-8866EE228BED
======================================================================

Shared with zsh [1104], R [x] [85910]:
      Dirty         Clean   Reclaimable    Regions    Category
        ---           ---           ---        ---    ---
        0 B           0 B           0 B         39    __DATA
        0 B           0 B           0 B         91    __TEXT
        ---           ---           ---        ---    ---
        0 B           0 B           0 B        130    TOTAL

======================================================================
Summary Footprint: 92155120 B
======================================================================

      Dirty         Clean   Reclaimable    Regions    Category
        ---           ---           ---        ---    ---
 90000000 B           0 B           0 B          5    MALLOC_SMALL
  2155120 B           0 B           0 B          8    MALLOC metadata
        ---           ---           ---        ---    ---
 92155120 B      573440 B           0 B        411    TOTAL
`

func TestParseFootprint(t *testing.T) {
	// The sample also holds a block for 713, which nobody asked for.
	want := map[int]bool{85910: true, 1104: true, 4242: true}
	got := parseFootprint([]byte(footprintSample), want)

	// The ledger's number wins over the header's; the header's is used for
	// the block that has no ledger; pid 713 was not asked for; pid 4242 has
	// no block.
	expected := map[int]int64{85910: 1671504, 1104: 500000}
	if !reflect.DeepEqual(got, expected) {
		t.Errorf("parseFootprint() = %v, want %v", got, expected)
	}

	// Asked for, 713 is read from its own block and does not take the
	// numbers of the blocks around it.
	got = parseFootprint([]byte(footprintSample), map[int]bool{713: true})
	if !reflect.DeepEqual(got, map[int]int64{713: 91000000}) {
		t.Errorf("parseFootprint() for 713 = %v, want 91000000", got)
	}
}

func TestParseFootprint_NothingToRead(t *testing.T) {
	usage := "Usage: footprint [options] <pid | name>\n\n  -f, --format <fmt>  bytes\n   --pid <pid>   a process by ID\n"
	for name, out := range map[string]string{
		"usage text": usage,
		"empty":      "",
		"garbage":    "\x00\x01 [12]: Footprint: x B\nFootprint: 5 B\n  [3]: Footprint: 4 B\n",
	} {
		if got := parseFootprint([]byte(out), map[int]bool{12: true, 3: true}); len(got) != 0 {
			t.Errorf("%s: parseFootprint() = %v, want nothing", name, got)
		}
	}
}

// fakeFootprint replaces footprintRun and records the arguments of each call.
func fakeFootprint(t *testing.T, out string, err error) *[][]string {
	t.Helper()
	var calls [][]string
	orig := footprintRun
	t.Cleanup(func() { footprintRun = orig })
	footprintRun = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte(out), err
	}
	return &calls
}

func TestPlatformMemory_OneCallWithAPidFlagForEach(t *testing.T) {
	calls := fakeFootprint(t, footprintSample, nil)

	got, err := platformTreeMemory(alone([]int{1104, 85910, 4242}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (map[int]int64{1104: 500000, 85910: 1671504}); !reflect.DeepEqual(got, want) {
		t.Errorf("platformTreeMemory() = %v, want %v", got, want)
	}
	want := [][]string{{"-f", "bytes", "--pid", "1104", "--pid", "85910", "--pid", "4242"}}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("footprint calls = %v, want %v", *calls, want)
	}
}

// footprint exits non-zero when a process could not be read, and the ones it
// could read are on its stdout all the same.
func TestPlatformMemory_ExitErrorStillYieldsTheBlocksRead(t *testing.T) {
	failed := exec.Command("sh", "-c", "exit 66").Run()
	var ee *exec.ExitError
	if !errors.As(failed, &ee) {
		t.Fatalf("setup: %v is not an ExitError", failed)
	}
	fakeFootprint(t, footprintSample, failed)

	got, err := platformTreeMemory(alone([]int{85910, 1}))
	if err != nil {
		t.Fatalf("error = %v, want none", err)
	}
	if want := (map[int]int64{85910: 1671504}); !reflect.DeepEqual(got, want) {
		t.Errorf("platformTreeMemory() = %v, want %v", got, want)
	}
}

func TestPlatformMemory_FailureToRunIsAnError(t *testing.T) {
	boom := errors.New("fork/exec /usr/bin/footprint: no such file or directory")
	fakeFootprint(t, footprintSample, boom)

	got, err := platformTreeMemory(alone([]int{85910}))
	if !errors.Is(err, boom) || got != nil {
		t.Errorf("platformTreeMemory() = %v, %v, want nothing and %v", got, err, boom)
	}

	fakeFootprint(t, "", context.DeadlineExceeded)
	got, err = platformTreeMemory(alone([]int{85910}))
	if !errors.Is(err, context.DeadlineExceeded) || got != nil {
		t.Errorf("platformTreeMemory() = %v, %v, want nothing and the deadline", got, err)
	}
}

// A footprint that hangs is cut off at footprintTimeout, and the real runner
// is what reports it as the deadline: the kill itself looks like any other
// failed exit. Memory then leaves the PIDs out and does not ask ps.
func TestPlatformMemory_TimeoutIsTheDeadlineAndNotPs(t *testing.T) {
	script := filepath.Join(t.TempDir(), "footprint")
	// exec, so that the kill reaches sleep itself and not a shell around it.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath, origTimeout, origMeasure, origPS := footprintPath, footprintTimeout, measureTrees, psOutput
	t.Cleanup(func() {
		footprintPath, footprintTimeout, measureTrees, psOutput = origPath, origTimeout, origMeasure, origPS
	})
	footprintPath, footprintTimeout = script, 50*time.Millisecond
	measureTrees = platformTreeMemory
	psCalls := 0
	psOutput = func(args ...string) ([]byte, error) {
		psCalls++
		return []byte("1 100\n"), nil
	}

	start := time.Now()
	got, err := platformTreeMemory(alone([]int{os.Getpid()}))
	if !errors.Is(err, context.DeadlineExceeded) || got != nil {
		t.Errorf("platformTreeMemory() = %v, %v, want nothing and the deadline", got, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %v, want it cut off at %v", elapsed, footprintTimeout)
	}

	got, err = TreeMemory(alone([]int{os.Getpid()}))
	if !errors.Is(err, context.DeadlineExceeded) || len(got) != 0 {
		t.Errorf("TreeMemory() = %v, %v, want nothing and the deadline", got, err)
	}
	if psCalls != 0 {
		t.Errorf("ps was called %d times after a timeout, want none", psCalls)
	}
}

// TestPlatformMemory_RealFootprint runs the real footprint, which is the part
// that differs between versions of macOS: its options and its output.
func TestPlatformMemory_RealFootprint(t *testing.T) {
	if _, err := os.Stat(footprintPath); err != nil {
		t.Skipf("no footprint here: %v", err)
	}

	pids := []int{os.Getpid()}
	for i := 0; i < 2; i++ {
		c := exec.Command("sleep", "30")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Process.Kill(); c.Wait() })
		pids = append(pids, c.Process.Pid)
	}

	// A PID that existed a moment ago, beside live ones: footprint reports
	// it on stderr and measures the others.
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	gone := exited.Process.Pid

	got, err := platformTreeMemory(alone(append(pids, gone)))
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range pids {
		if got[pid] <= 0 {
			t.Errorf("platformTreeMemory()[%d] = %d, want a positive size", pid, got[pid])
		}
	}
	if _, ok := got[gone]; ok {
		t.Errorf("platformTreeMemory() has an entry for the exited pid %d", gone)
	}
}

func TestParseFootprintOutput_Summary(t *testing.T) {
	blocks, summary, ok := parseFootprintOutput([]byte(footprintSample), map[int]bool{85910: true, 1104: true})
	if !ok || summary != 92155120 {
		t.Errorf("summary = %d, %v, want 92155120", summary, ok)
	}
	if want := (map[int]int64{85910: 1671504, 1104: 500000}); !reflect.DeepEqual(blocks, want) {
		t.Errorf("blocks = %v, want %v", blocks, want)
	}

	// One process read, as footprint prints it: no total.
	one := footprintSample[:strings.Index(footprintSample, "R [x]")]
	if _, _, ok := parseFootprintOutput([]byte(one), map[int]bool{713: true}); ok {
		t.Error("a Summary was found in output that has none")
	}
}

// footprintByCall stands in for footprint: out is chosen from the pids named
// in the arguments, and each call is recorded.
func footprintByCall(t *testing.T, answer func(args []string) (string, error)) *[][]string {
	t.Helper()
	var calls [][]string
	orig := footprintRun
	t.Cleanup(func() { footprintRun = orig })
	footprintRun = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		out, err := answer(args)
		return []byte(out), err
	}
	return &calls
}

// Copies of one process are measured together in one call; a copy with workers
// has a call of its own and takes the de-duplicated total.
func TestExactTreeMemory_Darwin(t *testing.T) {
	calls := footprintByCall(t, func(args []string) (string, error) { return footprintSample, nil })

	got, err := ExactTreeMemory([][]int{{713}, {85910, 1104}, {4242}})
	if err != nil {
		t.Fatal(err)
	}
	// 713 and 4242 are singles, in one call; 4242 has no block. The copy with a
	// worker is the Summary, not 1671504 + 500000.
	if want := (map[int]int64{713: 91000000, 85910: 92155120}); !reflect.DeepEqual(got, want) {
		t.Errorf("ExactTreeMemory() = %v, want %v", got, want)
	}
	want := [][]string{
		{"-f", "bytes", "--pid", "713", "--pid", "4242"},
		{"-f", "bytes", "--pid", "85910", "--pid", "1104"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("footprint calls = %v, want %v", *calls, want)
	}
}

// footprint reads one process only when the other is gone: it prints no total,
// and the copy is that one block. If it is the worker that was read and the
// leader is gone, the copy is not measured.
func TestExactTreeMemory_DarwinOneBlockAndMissingLeader(t *testing.T) {
	leaderOnly := footprintSample[strings.Index(footprintSample, "R [x]"):strings.Index(footprintSample, "zsh [1104]: 64-bit")]
	workerOnly := footprintSample[strings.Index(footprintSample, "zsh [1104]: 64-bit"):strings.Index(footprintSample, "Shared Cache")]
	footprintByCall(t, func(args []string) (string, error) {
		if args[3] == "85910" {
			return "======\n" + leaderOnly, nil
		}
		return "======\n" + workerOnly, nil
	})

	got, err := ExactTreeMemory([][]int{{85910, 1104}})
	if err != nil || !reflect.DeepEqual(got, map[int]int64{85910: 1671504}) {
		t.Errorf("leader read alone: %v, %v, want its phys_footprint", got, err)
	}
	got, err = ExactTreeMemory([][]int{{9999, 1104}})
	if err != nil || len(got) != 0 {
		t.Errorf("leader not read: %v, %v, want the copy absent", got, err)
	}
}

// A measurement is cut off as a whole: copies with workers are one call each,
// and the calls share one deadline, so a slow machine costs footprintTimeout
// and not footprintTimeout per copy.
func TestExactTreeMemory_DarwinOneDeadline(t *testing.T) {
	origTimeout := footprintTimeout
	t.Cleanup(func() { footprintTimeout = origTimeout })
	footprintTimeout = 150 * time.Millisecond

	orig := footprintRun
	t.Cleanup(func() { footprintRun = orig })
	var calls int
	footprintRun = func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		<-ctx.Done()
		return nil, ctx.Err()
	}

	start := time.Now()
	got, err := ExactTreeMemory([][]int{{1, 11}, {2, 21}, {3, 31}})
	if !errors.Is(err, context.DeadlineExceeded) || got != nil {
		t.Errorf("ExactTreeMemory() = %v, %v, want nothing and the deadline", got, err)
	}
	if elapsed := time.Since(start); elapsed > 2*footprintTimeout {
		t.Errorf("took %s for three copies, want about one timeout (%s)", elapsed, footprintTimeout)
	}
	if calls != 1 {
		t.Errorf("%d calls, want the measurement to end at the first that was cut off", calls)
	}
}

// The real footprint over a process and a child of it: the total is at least
// the parent's, and the copy is present.
func TestExactTreeMemory_RealFootprint(t *testing.T) {
	if _, err := os.Stat(footprintPath); err != nil {
		t.Skipf("no footprint here: %v", err)
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })

	got, err := ExactTreeMemory([][]int{{os.Getpid(), child.Process.Pid}})
	if err != nil {
		t.Fatal(err)
	}
	alone, err := ExactTreeMemory([][]int{{os.Getpid()}})
	if err != nil {
		t.Fatal(err)
	}
	if got[os.Getpid()] <= 0 || got[os.Getpid()] < alone[os.Getpid()]/2 {
		t.Errorf("tree = %d, process alone = %d, want a total of the same order or more", got[os.Getpid()], alone[os.Getpid()])
	}
}
