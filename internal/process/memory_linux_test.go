package process

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// procStatus is the part of a /proc/<pid>/status that holds the memory.
func procStatus(lines ...string) string {
	out := "Name:\tR\nState:\tS (sleeping)\nPid:\t42\n"
	for _, l := range lines {
		out += l + "\n"
	}
	return out + "Threads:\t1\n"
}

// fakeProcStatus makes a /proc with a status file for each of the processes;
// a pid with an empty status gets a directory and no file.
func fakeProcStatus(t *testing.T, statuses map[int]string) {
	t.Helper()
	fakeProcFiles(t, statuses, nil)
}

// fakeProcFiles is fakeProcStatus with a smaps_rollup for some of the processes.
func fakeProcFiles(t *testing.T, statuses, rollups map[int]string) {
	t.Helper()
	dir := t.TempDir()
	orig := procDir
	procDir = dir
	t.Cleanup(func() { procDir = orig })
	for pid, rollup := range rollups {
		pdir := filepath.Join(dir, strconv.Itoa(pid))
		if err := os.MkdirAll(pdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pdir, "smaps_rollup"), []byte(rollup), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for pid, status := range statuses {
		pdir := filepath.Join(dir, strconv.Itoa(pid))
		if err := os.MkdirAll(pdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if status != "" {
			if err := os.WriteFile(filepath.Join(pdir, "status"), []byte(status), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestParseProcStatus(t *testing.T) {
	tests := []struct {
		name string
		data string
		want int64
		ok   bool
	}{
		{"resident and swapped", procStatus("VmRSS:\t    1024 kB", "VmSwap:\t    2048 kB"), 3 << 20, true},
		{"nothing swapped", procStatus("VmRSS:\t    1024 kB", "VmSwap:\t       0 kB"), 1 << 20, true},
		{"no VmSwap line", procStatus("VmRSS:\t    1024 kB"), 1 << 20, true},
		{"a zombie has no memory lines", procStatus(), 0, false},
		{"swap alone is not a measure", procStatus("VmSwap:\t    2048 kB"), 0, false},
		{"a value that isn't one", procStatus("VmRSS:\t    lots kB", "VmSwap:\t    2048 kB"), 0, false},
		{"a unit that isn't kB", procStatus("VmRSS:\t    1024 MB"), 0, false},
		{"empty", "", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseProcStatus([]byte(tt.data))
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: parseProcStatus() = %d, %v, want %d, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestPlatformMemory_ReadsTheStatusOfEachProcess(t *testing.T) {
	fakeProcStatus(t, map[int]string{
		1: procStatus("VmRSS:\t    1024 kB", "VmSwap:\t    2048 kB"),
		2: procStatus("VmRSS:\t     512 kB"),
		3: procStatus(), // a zombie
		4: "",           // a directory without a status
	})

	got, err := platformMemory([]int{1, 2, 3, 4, 5})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int64{1: 3 << 20, 2: 512 * 1024}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("platformTreeMemory() = %v, want %v", got, want)
	}
}

func TestPlatformMemory_RealProc(t *testing.T) {
	pid := os.Getpid()
	got, err := platformMemory([]int{pid})
	if err != nil {
		t.Fatal(err)
	}
	if got[pid] <= 0 {
		t.Errorf("platformMemory()[%d] = %d, want a positive size", pid, got[pid])
	}
}

// A zombie is stood in for by ps, and only the zombie.
func TestMemory_PsStandsInForAProcessWithoutMemoryLines(t *testing.T) {
	fakeProcStatus(t, map[int]string{
		1: procStatus("VmRSS:\t    1024 kB", "VmSwap:\t    2048 kB"),
		2: procStatus(),
	})
	origMeasure, origPS := measureTrees, psOutput
	t.Cleanup(func() { measureTrees, psOutput = origMeasure, origPS })
	measureTrees = platformTreeMemory
	var calls [][]string
	psOutput = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("2 7\n"), nil
	}

	got, err := TreeMemory(alone([]int{1, 2}))
	if want := (map[int]int64{1: 3 << 20, 2: 7 * 1024}); !reflect.DeepEqual(got, want) {
		t.Errorf("TreeMemory() = %v, want %v", got, want)
	}
	if len(calls) != 1 || calls[0][len(calls[0])-1] != "2" {
		t.Errorf("ps calls = %v, want exactly one for pid 2 alone", calls)
	}
	if err == nil || !strings.HasSuffix(err.Error(), "pid 2") {
		t.Errorf("error = %v, want one naming pid 2", err)
	}
}

// rollup is a /proc/<pid>/smaps_rollup as Linux 6 prints it, with the
// proportional parts the parser must not add twice.
func rollup(pss, swapPss int) string {
	return "55d0c0a00000-7ffd9a5f3000 ---p 00000000 00:00 0                          [rollup]\n" +
		"Rss:              999999 kB\n" +
		"Pss:              " + strconv.Itoa(pss) + " kB\n" +
		"Pss_Dirty:         88888 kB\n" +
		"Pss_Anon:          77777 kB\n" +
		"Pss_File:          66666 kB\n" +
		"Pss_Shmem:             0 kB\n" +
		"Shared_Clean:       4096 kB\n" +
		"Swap:               1111 kB\n" +
		"SwapPss:          " + strconv.Itoa(swapPss) + " kB\nLocked:                 0 kB\n"
}

func TestParseSmapsRollup(t *testing.T) {
	tests := []struct {
		name string
		data string
		want int64
		ok   bool
	}{
		{"proportional and swapped", rollup(1000, 24), 1024 << 10, true},
		{"nothing swapped", rollup(1000, 0), 1000 << 10, true},
		{"no SwapPss line", "Rss: 5 kB\nPss: 7 kB\n", 7 << 10, true},
		{"no Pss is not a measure", "Rss: 5 kB\nSwapPss: 7 kB\n", 0, false},
		{"a value that isn't one", "Pss: lots kB\nSwapPss: 7 kB\n", 0, false},
		{"a unit that isn't kB", "Pss: 7 MB\n", 0, false},
		{"empty, as a zombie's is", "", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseSmapsRollup([]byte(tt.data))
		if got != tt.want || ok != tt.ok {
			t.Errorf("%s: parseSmapsRollup() = %d, %v, want %d, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
}

func TestPlatformMemory_PrefersPssAndFallsBackToTheStatus(t *testing.T) {
	fakeProcFiles(t, map[int]string{
		1: procStatus("VmRSS:\t    9000 kB", "VmSwap:\t       0 kB"), // has a rollup too: Pss wins
		2: procStatus("VmRSS:\t    1024 kB", "VmSwap:\t    2048 kB"), // no rollup: the status
		3: procStatus("VmRSS:\t    2048 kB"),                         // an unusable rollup: the status
		4: "",                                                        // a rollup and no status
	}, map[int]string{
		1: rollup(300, 12),
		3: "garbage\n",
		4: rollup(10, 0),
	})
	got, err := platformMemory([]int{1, 2, 3, 4, 5})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int64{1: 312 << 10, 2: 3 << 20, 3: 2 << 20, 4: 10 << 10}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("platformTreeMemory() = %v, want %v", got, want)
	}
}

func TestExactTreeMemory_Linux(t *testing.T) {
	fakeProcFiles(t, map[int]string{
		10: procStatus("VmRSS:\t   10240 kB"), // the leader, without a rollup
		11: "",
		12: "",
		20: procStatus(), // a zombie leader
		21: "",
		30: procStatus("VmRSS:\t    2048 kB"), // a leader alone
	}, map[int]string{
		11: rollup(2000, 0),
		12: rollup(3000, 1000),
		21: rollup(5000, 0),
	})
	// Tree 10 has the worker 11, the worker 12 and a process 13 that exited:
	// the sum leaves 13 out. Tree 20's leader is a zombie: absent. 40 does not exist.
	got, err := ExactTreeMemory([][]int{{10, 11, 12, 13}, {20, 21}, {30}, {40}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int64{10: (10240 + 2000 + 3000 + 1000) << 10, 30: 2048 << 10}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExactTreeMemory() = %v, want %v", got, want)
	}
}
