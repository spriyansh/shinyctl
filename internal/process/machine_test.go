package process

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The /proc/meminfo of a Raspberry Pi 5 with 4 GB and zram swap.
const piMeminfo = `MemTotal:        4146240 kB
MemFree:         1475008 kB
MemAvailable:    3239984 kB
Buffers:           93888 kB
Cached:          1598768 kB
SwapCached:            0 kB
Active:           383168 kB
Inactive:        1952496 kB
Active(anon):     158864 kB
Inactive(anon):   496624 kB
Active(file):     224304 kB
Inactive(file):  1455872 kB
Unevictable:           0 kB
Mlocked:               0 kB
SwapTotal:       2097136 kB
SwapFree:        2097088 kB
Zswap:                 0 kB
Zswapped:              0 kB
Dirty:               112 kB
Writeback:             0 kB
AnonPages:        644096 kB
Mapped:            91440 kB
Shmem:             12768 kB
KReclaimable:     158128 kB
Slab:             228736 kB
SReclaimable:     158128 kB
SUnreclaim:        70608 kB
KernelStack:        3440 kB
PageTables:         4768 kB
SecPageTables:       112 kB
NFS_Unstable:          0 kB
Bounce:                0 kB
WritebackTmp:          0 kB
CommitLimit:     4170256 kB
Committed_AS:    2090080 kB
VmallocTotal:   68447887360 kB
VmallocUsed:       44944 kB
VmallocChunk:          0 kB
Percpu:             3840 kB
CmaTotal:          65536 kB
CmaFree:            6464 kB
Balloon:               0 kB
`

func TestParseMeminfo(t *testing.T) {
	m, err := parseMeminfo([]byte(piMeminfo))
	if err != nil {
		t.Fatal(err)
	}
	want := MachineMemory{
		TotalBytes:     4146240 * 1024,
		AvailableBytes: 3239984 * 1024,
		SwapTotalBytes: 2097136 * 1024,
		SwapUsedBytes:  (2097136 - 2097088) * 1024,
	}
	if m != want {
		t.Errorf("got %+v, want %+v", m, want)
	}

	noSwap := "MemTotal: 1000 kB\nMemAvailable: 400 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"
	m, err = parseMeminfo([]byte(noSwap))
	if err != nil || m.SwapTotalBytes != 0 || m.SwapUsedBytes != 0 || m.AvailableBytes != 400*1024 {
		t.Errorf("no swap: got %+v, %v", m, err)
	}

	for name, in := range map[string]string{
		"no MemAvailable": "MemTotal: 1000 kB\nMemFree: 10 kB\n",
		"no MemTotal":     "MemAvailable: 1000 kB\n",
		"garbage":         "this is not\nmeminfo at all\n:::\nMemTotal: lots kB\n",
		"empty":           "",
	} {
		if _, err := parseMeminfo([]byte(in)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParsePressure(t *testing.T) {
	const normal = "some avg10=0.00 avg60=23.45 avg300=1.00 total=12345\nfull avg10=0.00 avg60=4.00 avg300=0.50 total=999\n"
	for _, tc := range []struct {
		name string
		in   string
		want float64
		ok   bool
	}{
		{"normal", normal, 23.45, true},
		{"full first", "full avg10=0.00 avg60=4.00 avg300=0.50 total=9\nsome avg10=0.00 avg60=7.50 avg300=1.00 total=1\n", 7.5, true},
		{"only full", "full avg10=0.00 avg60=4.00 avg300=0.50 total=9\n", 0, false},
		{"no avg60", "some avg10=0.00 avg300=1.00 total=1\n", 0, false},
		{"not a number", "some avg10=0.00 avg60=lots avg300=1.00 total=1\n", 0, false},
		{"empty", "", 0, false},
		{"garbage", "hello\nworld there\n", 0, false},
	} {
		got, ok := parsePressure([]byte(tc.in))
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestReadMachineFromProc(t *testing.T) {
	write := func(t *testing.T, dir, name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("meminfo alone", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "meminfo", piMeminfo)
		m, err := readMachineFromProc(dir)
		if err != nil || m.StallKnown || m.TotalBytes != 4146240*1024 {
			t.Errorf("got %+v, %v", m, err)
		}
	})
	t.Run("with pressure", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "meminfo", piMeminfo)
		write(t, dir, "pressure/memory", "some avg10=0.00 avg60=12.50 avg300=1.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
		m, err := readMachineFromProc(dir)
		if err != nil || !m.StallKnown || m.StallPercent != 12.5 {
			t.Errorf("got %+v, %v", m, err)
		}
	})
	t.Run("pressure unreadable", func(t *testing.T) {
		dir := t.TempDir()
		write(t, dir, "meminfo", piMeminfo)
		if err := os.MkdirAll(filepath.Join(dir, "pressure", "memory"), 0o755); err != nil {
			t.Fatal(err)
		}
		m, err := readMachineFromProc(dir)
		if err != nil || m.StallKnown || m.AvailableBytes != 3239984*1024 {
			t.Errorf("got %+v, %v", m, err)
		}
	})
	t.Run("meminfo missing", func(t *testing.T) {
		if _, err := readMachineFromProc(t.TempDir()); err == nil {
			t.Error("no error")
		}
	})
}

func swapusage(total, avail, used uint64) []byte {
	b := make([]byte, 32)
	binary.NativeEndian.PutUint64(b[0:], total)
	binary.NativeEndian.PutUint64(b[8:], avail)
	binary.NativeEndian.PutUint64(b[16:], used)
	binary.NativeEndian.PutUint32(b[24:], 16384)
	return b
}

func TestMachineFromSysctl(t *testing.T) {
	const gib = 1 << 30
	swap := swapusage(14*gib, 2*gib, 12*gib)

	m := machineFromSysctl(2, 10, 32*gib, swap)
	if m.Pressure != "warning" || m.TotalBytes != 32*gib || m.SwapTotalBytes != 14*gib || m.SwapUsedBytes != 12*gib {
		t.Errorf("got %+v", m)
	}
	if want := int64(32 * gib / 10); m.AvailableBytes < want-gib/10 || m.AvailableBytes > want+gib/10 {
		t.Errorf("AvailableBytes = %d, want about %d", m.AvailableBytes, want)
	}

	for level, want := range map[uint32]string{1: "normal", 2: "warning", 4: "critical", 3: "unknown", 0: "unknown"} {
		if got := machineFromSysctl(level, 50, 8*gib, nil).Pressure; got != want {
			t.Errorf("pressure %d = %q, want %q", level, got, want)
		}
	}

	for name, raw := range map[string][]byte{"nil": nil, "empty": {}, "short": swap[:16], "long": append(swap, 0)} {
		m := machineFromSysctl(1, 50, 8*gib, raw)
		if m.SwapTotalBytes != 0 || m.SwapUsedBytes != 0 {
			t.Errorf("%s swapusage: swap %+v, want none", name, m)
		}
	}

	if m := machineFromSysctl(1, 0, 8*gib, nil); m.AvailableBytes != 0 || m.TotalBytes != 8*gib {
		t.Errorf("level 0: got %+v", m)
	}
}

// CI's macOS runners are virtual machines, so only that something is read is
// asserted.
func TestReadMachineMemory_Real(t *testing.T) {
	m, err := ReadMachineMemory()
	if errors.Is(err, ErrNoMachineMemory) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if m.TotalBytes <= 0 {
		t.Errorf("TotalBytes = %d", m.TotalBytes)
	}
}
