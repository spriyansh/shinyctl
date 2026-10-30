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
	dir := t.TempDir()
	orig := procDir
	procDir = dir
	t.Cleanup(func() { procDir = orig })
	for pid, status := range statuses {
		pdir := filepath.Join(dir, strconv.Itoa(pid))
		if err := os.Mkdir(pdir, 0o755); err != nil {
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
		t.Errorf("platformMemory() = %v, want %v", got, want)
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
	origMeasure, origPS := measure, psOutput
	t.Cleanup(func() { measure, psOutput = origMeasure, origPS })
	measure = platformMemory
	var calls [][]string
	psOutput = func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("2 7\n"), nil
	}

	got, err := Memory([]int{1, 2})
	if want := (map[int]int64{1: 3 << 20, 2: 7 * 1024}); !reflect.DeepEqual(got, want) {
		t.Errorf("Memory() = %v, want %v", got, want)
	}
	if len(calls) != 1 || calls[0][len(calls[0])-1] != "2" {
		t.Errorf("ps calls = %v, want exactly one for pid 2 alone", calls)
	}
	if err == nil || !strings.HasSuffix(err.Error(), "pid 2") {
		t.Errorf("error = %v, want one naming pid 2", err)
	}
}
