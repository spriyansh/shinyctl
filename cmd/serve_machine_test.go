package cmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/process"
)

// linuxMachine is a Linux reading of a machine of 1000 units, so that tenths
// of a percent are whole numbers; avail is in tenths of a percent.
func linuxMachine(avail int64) process.MachineMemory {
	return process.MachineMemory{TotalBytes: 1000, AvailableBytes: avail}
}

func withStall(m process.MachineMemory, stall float64) process.MachineMemory {
	m.StallPercent, m.StallKnown = stall, true
	return m
}

func macMachine(pressure string) process.MachineMemory {
	return process.MachineMemory{TotalBytes: 32 << 30, AvailableBytes: 3 << 30, Pressure: pressure}
}

func machineLow(w *machineWatch) bool {
	got, ok := w.current()
	return ok && got.Low
}

func countLines(buf *logBuffer, sub string) int {
	return strings.Count(buf.String(), sub)
}

func TestMachineWatch_LogsEnteringAndLeavingOnce(t *testing.T) {
	buf := captureLog(t)
	var w machineWatch

	w.observe(linuxMachine(500), nil)
	if buf.String() != "" || machineLow(&w) {
		t.Fatalf("healthy: low %v, log %q", machineLow(&w), buf.String())
	}
	w.observe(linuxMachine(90), nil)
	w.observe(linuxMachine(80), nil)
	w.observe(linuxMachine(70), nil)
	if got := countLines(buf, "this machine is low on memory"); got != 1 {
		t.Errorf("%d entering lines, want 1:\n%s", got, buf.String())
	}
	if !machineLow(&w) {
		t.Error("not low")
	}
	w.observe(linuxMachine(400), nil)
	w.observe(linuxMachine(450), nil)
	if got := countLines(buf, "no longer low on memory"); got != 1 {
		t.Errorf("%d leaving lines, want 1:\n%s", got, buf.String())
	}
	if machineLow(&w) {
		t.Error("still low")
	}
}

func TestMachineWatch_AvailableThresholds(t *testing.T) {
	var w machineWatch
	captureLog(t)

	w.observe(linuxMachine(90), nil)
	if !machineLow(&w) {
		t.Error("9% is not low")
	}
	w.observe(linuxMachine(120), nil)
	if !machineLow(&w) {
		t.Error("12% from low is not still low")
	}
	w.observe(linuxMachine(149), nil)
	if !machineLow(&w) {
		t.Error("14.9% from low is not still low")
	}
	w.observe(linuxMachine(150), nil)
	if machineLow(&w) {
		t.Error("exactly 15% is not back")
	}

	var h machineWatch
	h.observe(linuxMachine(120), nil)
	if machineLow(&h) {
		t.Error("12% from healthy is low")
	}
	h.observe(linuxMachine(100), nil)
	if machineLow(&h) {
		t.Error("exactly 10% is low")
	}
	h.observe(linuxMachine(99), nil)
	if !machineLow(&h) {
		t.Error("9.9% is not low")
	}
}

func TestMachineWatch_Stall(t *testing.T) {
	captureLog(t)
	low := func(avail int64, stall float64, known bool) bool {
		var w machineWatch
		m := linuxMachine(avail)
		if known {
			m = withStall(m, stall)
		}
		w.observe(m, nil)
		return machineLow(&w)
	}

	if low(500, 20.0, true) {
		t.Error("50% available with stall 20 is low, the gate did not hold")
	}
	if !low(200, 20.0, true) {
		t.Error("20% available with stall 20.0 is not low")
	}
	if low(200, 19.99, true) {
		t.Error("20% available with stall 19.99 is low")
	}
	if low(250, 30, true) {
		t.Error("exactly 25% available is inside the gate")
	}
	if !low(90, 0, false) {
		t.Error("9% available with an unknown stall is not low")
	}

	t.Run("leaves when the stall is under 10", func(t *testing.T) {
		buf := captureLog(t)
		var w machineWatch
		w.observe(withStall(linuxMachine(120), 25), nil)
		if !machineLow(&w) {
			t.Fatal("not low on the stall")
		}
		w.observe(withStall(linuxMachine(120), 10), nil)
		if !machineLow(&w) {
			t.Error("stall 10 should hold it")
		}
		w.observe(withStall(linuxMachine(120), 9.9), nil)
		if machineLow(&w) {
			t.Error("still low at stall 9.9")
		}
		if got := countLines(buf, "no longer low"); got != 1 {
			t.Errorf("%d leaving lines, want 1:\n%s", got, buf.String())
		}
		if strings.Contains(lastLine(buf), "waiting for memory") {
			t.Errorf("the leaving line carries the stall:\n%s", buf.String())
		}
	})

	t.Run("low on available leaves at 15% whatever the stall", func(t *testing.T) {
		captureLog(t)
		var w machineWatch
		w.observe(withStall(linuxMachine(90), 15), nil)
		w.observe(withStall(linuxMachine(140), 15), nil)
		if !machineLow(&w) {
			t.Fatal("left at 14%")
		}
		w.observe(withStall(linuxMachine(150), 15), nil)
		if machineLow(&w) {
			t.Error("a stall of 15 that never entered kept it low")
		}
	})

	t.Run("the gate ends a spell on the stall", func(t *testing.T) {
		captureLog(t)
		var w machineWatch
		w.observe(withStall(linuxMachine(200), 40), nil)
		w.observe(withStall(linuxMachine(300), 15), nil)
		if machineLow(&w) {
			t.Error("still low at 30% available")
		}
	})

	t.Run("an unknown stall clears it", func(t *testing.T) {
		captureLog(t)
		var w machineWatch
		w.observe(withStall(linuxMachine(200), 40), nil)
		w.observe(linuxMachine(200), nil)
		if machineLow(&w) {
			t.Error("still low without a stall")
		}
	})

	t.Run("both in one reading give one line", func(t *testing.T) {
		buf := captureLog(t)
		var w machineWatch
		w.observe(withStall(linuxMachine(90), 40), nil)
		if got := countLines(buf, "is low on memory"); got != 1 {
			t.Errorf("%d lines, want 1:\n%s", got, buf.String())
		}
	})

	t.Run("one clearing while the other holds says nothing", func(t *testing.T) {
		buf := captureLog(t)
		var w machineWatch
		w.observe(withStall(linuxMachine(90), 40), nil)
		w.observe(withStall(linuxMachine(120), 5), nil)
		if !machineLow(&w) {
			t.Error("not low on available")
		}
		if got := countLines(buf, "this machine"); got != 1 {
			t.Errorf("%d lines, want only the entering one:\n%s", got, buf.String())
		}
	})

	t.Run("a failed read keeps both flags", func(t *testing.T) {
		captureLog(t)
		var w machineWatch
		w.observe(withStall(linuxMachine(200), 40), nil)
		w.observe(process.MachineMemory{}, errors.New("boom"))
		w.Lock()
		both := w.lowStall && !w.lowAvail && !w.ok
		w.Unlock()
		if !both {
			t.Error("flags or ok wrong after a failed read")
		}
		if _, ok := w.current(); ok {
			t.Error("current is ok after a failed read")
		}
	})

	t.Run("macOS never sets the stall", func(t *testing.T) {
		captureLog(t)
		var w machineWatch
		m := withStall(macMachine("normal"), 90)
		w.observe(m, nil)
		w.Lock()
		stall, low := w.lowStall, w.lowAvail
		w.Unlock()
		if stall || low {
			t.Errorf("lowStall %v, lowAvail %v", stall, low)
		}
	})
}

func lastLine(buf *logBuffer) string {
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	return lines[len(lines)-1]
}

func TestMachineWatch_LogLines(t *testing.T) {
	buf := captureLog(t)
	var w machineWatch
	m := withStall(process.MachineMemory{
		TotalBytes: 4 << 30, AvailableBytes: 780 << 20, SwapTotalBytes: 2 << 30, SwapUsedBytes: 1900 << 20,
	}, 23.4)
	w.observe(m, nil)
	want := "memory: this machine is low on memory: 780M of 4.0G available, swap 1.9G of 2.0G used, a process was waiting for memory 23% of the last minute"
	if !strings.Contains(lastLine(buf), want) {
		t.Errorf("entering line = %q, want it to contain %q", lastLine(buf), want)
	}

	m.AvailableBytes = 2 << 30
	m.StallPercent = 9.6
	w.observe(m, nil)
	want = "memory: this machine is no longer low on memory: 2.0G of 4.0G available, swap 1.9G of 2.0G used"
	if got := lastLine(buf); !strings.HasSuffix(got, want) {
		t.Errorf("leaving line = %q, want it to end with %q", got, want)
	}
}

func TestMachineWatch_ReadFailures(t *testing.T) {
	buf := captureLog(t)
	var w machineWatch
	boom := errors.New("boom")

	w.observe(linuxMachine(90), nil)
	w.observe(process.MachineMemory{}, boom)
	if got, ok := w.current(); ok {
		t.Errorf("current after a failed read = %+v, true", got)
	}
	w.Lock()
	stillLow := w.lowAvail
	w.Unlock()
	if !stillLow {
		t.Error("a failed read ended the spell")
	}
	w.observe(process.MachineMemory{}, boom)
	if got := countLines(buf, "can't read the machine's memory: boom; status will not say when it is low"); got != 1 {
		t.Errorf("%d failure lines for two failures, want 1:\n%s", got, buf.String())
	}

	// Recovering at 12% from a low spell stays low, and says nothing.
	before := countLines(buf, "memory:")
	w.observe(linuxMachine(120), nil)
	if after := countLines(buf, "memory:"); after != before {
		t.Errorf("recovery logged:\n%s", buf.String())
	}
	if !machineLow(&w) {
		t.Error("not low at 12% after recovering from a low spell")
	}

	// Success, then failure, logs again.
	w.observe(process.MachineMemory{}, boom)
	if got := countLines(buf, "can't read"); got != 2 {
		t.Errorf("%d failure lines after a second failing spell, want 2:\n%s", got, buf.String())
	}
}

func TestMachineWatch_MacOS(t *testing.T) {
	buf := captureLog(t)
	var w machineWatch
	w.observe(macMachine("normal"), nil)
	if machineLow(&w) {
		t.Error("normal is low")
	}
	w.observe(macMachine("warning"), nil)
	if !machineLow(&w) {
		t.Error("warning is not low")
	}
	w.observe(macMachine("critical"), nil)
	if !machineLow(&w) {
		t.Error("critical ended it")
	}
	w.observe(macMachine("warning"), nil)
	if !machineLow(&w) {
		t.Error("warning after critical ended it")
	}
	w.observe(macMachine("normal"), nil)
	if machineLow(&w) {
		t.Error("normal did not end it")
	}
	if got := countLines(buf, "this machine"); got != 2 {
		t.Errorf("%d lines, want entering and leaving:\n%s", got, buf.String())
	}
}

func TestMachineWatch_NoSupport(t *testing.T) {
	buf := captureLog(t)
	var w machineWatch
	w.observe(process.MachineMemory{}, process.ErrNoMachineMemory)
	w.observe(process.MachineMemory{}, process.ErrNoMachineMemory)
	if buf.String() != "" {
		t.Errorf("logged: %s", buf.String())
	}
	if _, ok := w.current(); ok {
		t.Error("current is ok")
	}
}

func TestRunMachineLoop(t *testing.T) {
	var w machineWatch
	var mu sync.Mutex
	reads := 0
	read := func() (process.MachineMemory, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return linuxMachine(500), nil
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return reads
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runMachineLoop(ctx, 10*time.Millisecond, read, &w)
		close(done)
	}()
	waitUntil(t, "two readings", 5*time.Second, func() bool { return count() >= 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not return")
	}
	if _, ok := w.current(); !ok {
		t.Error("the loop's readings did not reach the watch")
	}
}

func TestDescribeMachine(t *testing.T) {
	const m = 1 << 20
	g := func(n float64) int64 { return int64(n * (1 << 30)) }
	for _, tc := range []struct {
		name string
		in   api.MachineMemory
		want string
	}{
		{"linux with swap", api.MachineMemory{Low: true, TotalBytes: g(3.9), AvailableBytes: 312 * m, SwapTotalBytes: g(2), SwapUsedBytes: g(1.8)},
			"this machine is low on memory: 312M of 3.9G available, swap 1.8G of 2.0G used"},
		{"unused swap", api.MachineMemory{Low: true, TotalBytes: g(4), AvailableBytes: 300 * m, SwapTotalBytes: g(2)},
			"this machine is low on memory: 300M of 4.0G available, swap 0 of 2.0G used"},
		{"no swap", api.MachineMemory{Low: true, TotalBytes: g(1.9), AvailableBytes: 100 * m},
			"this machine is low on memory: 100M of 1.9G available and no swap"},
		{"nothing available", api.MachineMemory{Low: true, TotalBytes: g(1.9)},
			"this machine is low on memory: 0 of 1.9G available and no swap"},
		{"stall 23", api.MachineMemory{Low: true, TotalBytes: g(1.9), AvailableBytes: 380 * m, StallPercent: 23},
			"this machine is low on memory: 380M of 1.9G available and no swap, a process was waiting for memory 23% of the last minute"},
		{"stall 10", api.MachineMemory{Low: true, TotalBytes: g(4), AvailableBytes: 300 * m, SwapTotalBytes: g(2), SwapUsedBytes: g(1), StallPercent: 10},
			"this machine is low on memory: 300M of 4.0G available, swap 1.0G of 2.0G used, a process was waiting for memory 10% of the last minute"},
		{"stall 9", api.MachineMemory{Low: true, TotalBytes: g(4), AvailableBytes: 300 * m, StallPercent: 9},
			"this machine is low on memory: 300M of 4.0G available and no swap"},
		{"not low with a stall", api.MachineMemory{TotalBytes: g(4), AvailableBytes: g(1.4), StallPercent: 23},
			"this machine is no longer low on memory: 1.4G of 4.0G available and no swap"},
		{"mac full", api.MachineMemory{Low: true, TotalBytes: g(32), AvailableBytes: g(2.88), SwapUsedBytes: g(12.4), Pressure: "warning"},
			"this machine is low on memory (memory pressure: warning): 9% of 32.0G free, swap 12.4G used"},
		{"mac without level", api.MachineMemory{Low: true, TotalBytes: g(32), SwapUsedBytes: g(12.4), Pressure: "warning"},
			"this machine is low on memory (memory pressure: warning): swap 12.4G used"},
		{"mac without swap", api.MachineMemory{Low: true, TotalBytes: g(32), AvailableBytes: g(2.88), Pressure: "critical"},
			"this machine is low on memory (memory pressure: critical): 9% of 32.0G free"},
		{"mac with neither", api.MachineMemory{Low: true, TotalBytes: g(32), Pressure: "warning"},
			"this machine is low on memory (memory pressure: warning)"},
		{"mac never shows a stall", api.MachineMemory{Low: true, TotalBytes: g(32), AvailableBytes: g(2.88), StallPercent: 50, Pressure: "warning"},
			"this machine is low on memory (memory pressure: warning): 9% of 32.0G free"},
		{"mac leaving", api.MachineMemory{TotalBytes: g(32), AvailableBytes: g(12.8), Pressure: "normal"},
			"this machine is no longer low on memory: 40% of 32.0G free"},
	} {
		if got := describeMachine(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}
