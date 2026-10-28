package process

import (
	"testing"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

func TestPickPort(t *testing.T) {
	always := func(int) bool { return true }
	never := func(int) bool { return false }

	t.Run("the lowest free port", func(t *testing.T) {
		if p, err := PickPort(nil, always, 0); err != nil || p != manifest.InternalPortMin {
			t.Errorf("PickPort = %d, %v, want %d", p, err, manifest.InternalPortMin)
		}
	})

	t.Run("skips taken ports", func(t *testing.T) {
		taken := map[int]bool{20000: true, 20001: true, 20003: true}
		if p, err := PickPort(taken, always, 0); err != nil || p != 20002 {
			t.Errorf("PickPort = %d, %v, want 20002", p, err)
		}
	})

	t.Run("skips ports that are not free", func(t *testing.T) {
		free := func(p int) bool { return p != 20000 && p != 20001 }
		if p, err := PickPort(nil, free, 0); err != nil || p != 20002 {
			t.Errorf("PickPort = %d, %v, want 20002", p, err)
		}
	})

	t.Run("fails when nothing is free", func(t *testing.T) {
		_, err := PickPort(nil, never, 0)
		want := "no free port left between 20000 and 29999 for the app's process"
		if err == nil || err.Error() != want {
			t.Errorf("PickPort error = %v, want %q", err, want)
		}
	})
}

func TestPickPort_StartsWhereToldAndWrapsAround(t *testing.T) {
	always := func(int) bool { return true }
	if p, err := PickPort(nil, always, 25000); err != nil || p != 25000 {
		t.Errorf("PickPort from 25000 = %d, %v, want 25000", p, err)
	}
	taken := map[int]bool{25000: true, 25001: true}
	if p, _ := PickPort(taken, always, 25000); p != 25002 {
		t.Errorf("PickPort skipping taken ports = %d, want 25002", p)
	}
	// Everything from the start to the top is gone: it wraps to the bottom.
	free := func(p int) bool { return p < 29998 }
	if p, err := PickPort(nil, free, 29998); err != nil || p != 20000 {
		t.Errorf("PickPort wrapping around = %d, %v, want 20000", p, err)
	}
	// A start outside the range is brought into it.
	if p, _ := PickPort(nil, always, 5); p < 20000 || p > 29999 {
		t.Errorf("PickPort from 5 = %d, want a port in the range", p)
	}
	if p, _ := PickPort(nil, always, 30000); p != 20000 {
		t.Errorf("PickPort from 30000 = %d, want 20000", p)
	}
	only := func(p int) bool { return p == 24999 }
	if p, err := PickPort(nil, only, 25000); err != nil || p != 24999 {
		t.Errorf("PickPort with one free port just before the start = %d, %v, want 24999", p, err)
	}
}

func TestPortStart_IsPerUserAndStaysInTheRange(t *testing.T) {
	for uid := 0; uid < 3000; uid += 7 {
		for step := 0; step < startWindow; step += 17 {
			if p := portStartFor(uid, step); p < 20000 || p > 29999 {
				t.Fatalf("portStartFor(%d, %d) = %d, out of the range", uid, step, p)
			}
		}
	}
	if p := PortStart(); p < 20000 || p > 29999 {
		t.Errorf("PortStart() = %d, out of the range", p)
	}
}

// Neighbouring users, which is what a Mac gives (501, 502, ...) and what a
// server gives (1000, 1001, ...), get scan windows that don't overlap.
func TestPortStart_NeighbouringUsersDontShareAWindow(t *testing.T) {
	for _, r := range [][2]int{{501, 520}, {1000, 1030}} {
		for a := r[0]; a <= r[1]; a++ {
			for b := a + 1; b <= r[1]; b++ {
				d := portStartFor(a, 0) - portStartFor(b, 0)
				if d < 0 {
					d = -d
				}
				if d = min(d, 10000-d); d < startWindow {
					t.Errorf("uids %d and %d start %d ports apart, want at least %d", a, b, d, startWindow)
				}
			}
		}
	}
}
