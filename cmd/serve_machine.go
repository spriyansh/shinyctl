/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

// This file holds the watch over the memory of the whole machine: a loop of
// its own reads it every second, decides when it is low, and logs when that
// changes. The reading and the verdict are kept here, so that status and the
// log say the same.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/process"
)

// machineInterval is how often the machine's memory is read. It is a fixed
// gap and not the memory loop's, which grows when a measurement of the copies
// is slow, exactly when the machine is short of memory.
const machineInterval = time.Second

// The machine is low when less than lowAvailablePercent of its memory is
// available, and no longer low at backAvailablePercent, so that a reading
// near the line doesn't flip the note on and off.
const (
	lowAvailablePercent  = 10
	backAvailablePercent = 15
)

// The stall is the share of the last minute a process waited for memory
// (Linux's pressure stall information). It makes the machine low at
// lowStallPercent and keeps it so down to backStallPercent, but only while
// less than stallAvailablePercent of memory is available: the kernel adds up
// the stalls of every cgroup, so a container thrashing inside its own limit
// would otherwise count on a machine with memory to spare.
const (
	lowStallPercent       = 20.0
	backStallPercent      = 10.0
	stallAvailablePercent = 25
)

// machineReadFunc reads the machine's memory; a parameter of runMachineLoop
// so that tests pass their own.
type machineReadFunc func() (process.MachineMemory, error)

// machineWatch is the last reading of the machine's memory and the verdict on
// it. It is the one owner of that state, so that status, the log and apply
// agree. The two flags each have their own hysteresis, and the machine is low
// when either holds.
type machineWatch struct {
	sync.Mutex
	reading  process.MachineMemory
	ok       bool // reading is from the last read, which worked
	lowAvail bool
	lowStall bool
	failed   bool // the last read failed, and the log said so
}

// machine is the watch of shinyctl-api. Tests of the decision, the log and the
// loop use a machineWatch of their own.
var machine machineWatch

// observe takes the result of one read. A failed read keeps the verdict, as
// the machine did not get better by being unreadable, and is logged once per
// run of failures. A system that can't read at all says nothing.
func (w *machineWatch) observe(m process.MachineMemory, err error) {
	w.Lock()
	defer w.Unlock()

	if err != nil {
		w.ok = false
		if errors.Is(err, process.ErrNoMachineMemory) {
			return
		}
		if !w.failed {
			w.failed = true
			log.Printf("memory: can't read the machine's memory: %v; status will not say when it is low", err)
		}
		return
	}

	was := w.lowAvail || w.lowStall
	w.reading = m
	w.ok = true
	w.failed = false
	w.lowAvail = availLow(m, w.lowAvail)
	w.lowStall = stallLow(m, w.lowStall)

	if (w.lowAvail || w.lowStall) != was {
		log.Printf("memory: %s", describeMachine(w.apiLocked()))
	}
}

// availLow is whether less memory is available than the line, with the
// distance between going low and coming back. On macOS it is the kernel's own
// level: warning and critical are low, normal is back, and a level this code
// doesn't know leaves things as they were.
func availLow(m process.MachineMemory, was bool) bool {
	if m.Pressure != "" {
		switch m.Pressure {
		case "warning", "critical":
			return true
		case "normal":
			return false
		}
		return was
	}
	if was {
		return m.AvailableBytes*100 < backAvailablePercent*m.TotalBytes
	}
	return m.AvailableBytes*100 < lowAvailablePercent*m.TotalBytes
}

// stallLow is whether processes wait for memory so much that the machine is
// low, see the constants. An unknown stall counts as none.
func stallLow(m process.MachineMemory, was bool) bool {
	if m.Pressure != "" || !m.StallKnown {
		return false
	}
	if m.AvailableBytes*100 >= stallAvailablePercent*m.TotalBytes {
		return false
	}
	if was {
		return m.StallPercent >= backStallPercent
	}
	return m.StallPercent >= lowStallPercent
}

// apiLocked is the reading as the API carries it; w must be locked.
func (w *machineWatch) apiLocked() api.MachineMemory {
	m := w.reading
	return api.MachineMemory{
		Low:            w.lowAvail || w.lowStall,
		TotalBytes:     m.TotalBytes,
		AvailableBytes: m.AvailableBytes,
		SwapTotalBytes: m.SwapTotalBytes,
		SwapUsedBytes:  m.SwapUsedBytes,
		StallPercent:   int(m.StallPercent),
		Pressure:       m.Pressure,
	}
}

// current is the last reading with the verdict, and false when there is none:
// the last read failed, or nothing could be read yet.
func (w *machineWatch) current() (api.MachineMemory, bool) {
	w.Lock()
	defer w.Unlock()
	if !w.ok {
		return api.MachineMemory{}, false
	}
	return w.apiLocked(), true
}

// currentIfLow is the reading when the machine is low, for the replies of
// apply and restart; nil when it is healthy or there is no reading.
func (w *machineWatch) currentIfLow() *api.MachineMemory {
	if m, ok := w.current(); ok && m.Low {
		return &m
	}
	return nil
}

// set is for tests: a reading, and the verdict on it, without going through
// observe, which logs.
func (w *machineWatch) set(reading process.MachineMemory, low bool) {
	w.Lock()
	defer w.Unlock()
	w.reading = reading
	w.ok = true
	w.lowAvail = low
	w.lowStall = low
	w.failed = false
}

// clear is for tests: no reading, as before the first one.
func (w *machineWatch) clear() {
	w.Lock()
	defer w.Unlock()
	w.reading = process.MachineMemory{}
	w.ok = false
	w.lowAvail = false
	w.lowStall = false
	w.failed = false
}

// runMachineLoop reads the machine's memory every interval until ctx ends.
func runMachineLoop(ctx context.Context, interval time.Duration, read machineReadFunc, w *machineWatch) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.observe(read())
		}
	}
}

// describeMachine says how the machine stands, without a capital or a full
// stop, for the log, status and the warning of apply: "this machine is low on
// memory: 312M of 3.9G available, swap 1.8G of 2.0G used". For a reading that
// is not low it says that the machine is no longer low. A 0 prints as "0"
// where formatMem prints "-".
func describeMachine(m api.MachineMemory) string {
	if m.Pressure != "" {
		return describeMachineMac(m)
	}

	line := "this machine is low on memory"
	if !m.Low {
		line = "this machine is no longer low on memory"
	}
	line += fmt.Sprintf(": %s of %s available", machineBytes(m.AvailableBytes), formatMem(m.TotalBytes))
	if m.SwapTotalBytes > 0 {
		line += fmt.Sprintf(", swap %s of %s used", machineBytes(m.SwapUsedBytes), formatMem(m.SwapTotalBytes))
	} else {
		line += " and no swap"
	}
	if m.Low && m.StallPercent >= int(backStallPercent) {
		line += fmt.Sprintf(", a process was waiting for memory %d%% of the last minute", m.StallPercent)
	}
	return line
}

// describeMachineMac is describeMachine for macOS, where the kernel's own
// level says whether memory is low, the free share is derived from it, and
// swap is a size that grows as needed, so only what is used is shown.
func describeMachineMac(m api.MachineMemory) string {
	line := "this machine is low on memory"
	if m.Low {
		line += fmt.Sprintf(" (memory pressure: %s)", m.Pressure)
	} else {
		line = "this machine is no longer low on memory"
	}

	var parts []string
	if m.AvailableBytes > 0 && m.TotalBytes > 0 {
		percent := int(math.Round(100 * float64(m.AvailableBytes) / float64(m.TotalBytes)))
		parts = append(parts, fmt.Sprintf("%d%% of %s free", percent, formatMem(m.TotalBytes)))
	}
	if m.SwapUsedBytes > 0 {
		parts = append(parts, fmt.Sprintf("swap %s used", formatMem(m.SwapUsedBytes)))
	}
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	return line
}

// machineBytes is formatMem, except that zero bytes print as "0".
func machineBytes(n int64) string {
	if n == 0 {
		return "0"
	}
	return formatMem(n)
}
