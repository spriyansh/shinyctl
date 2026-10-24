package process

import (
	"testing"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

func TestPickPort(t *testing.T) {
	always := func(int) bool { return true }
	never := func(int) bool { return false }

	t.Run("the lowest free port", func(t *testing.T) {
		if p, err := PickPort(nil, always); err != nil || p != manifest.InternalPortMin {
			t.Errorf("PickPort = %d, %v, want %d", p, err, manifest.InternalPortMin)
		}
	})

	t.Run("skips taken ports", func(t *testing.T) {
		taken := map[int]bool{20000: true, 20001: true, 20003: true}
		if p, err := PickPort(taken, always); err != nil || p != 20002 {
			t.Errorf("PickPort = %d, %v, want 20002", p, err)
		}
	})

	t.Run("skips ports that are not free", func(t *testing.T) {
		free := func(p int) bool { return p != 20000 && p != 20001 }
		if p, err := PickPort(nil, free); err != nil || p != 20002 {
			t.Errorf("PickPort = %d, %v, want 20002", p, err)
		}
	})

	t.Run("fails when nothing is free", func(t *testing.T) {
		_, err := PickPort(nil, never)
		want := "no free port left between 20000 and 29999 for the app's process"
		if err == nil || err.Error() != want {
			t.Errorf("PickPort error = %v, want %q", err, want)
		}
	})
}
