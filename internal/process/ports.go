package process

import (
	"fmt"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

// PickPort returns the lowest port in the internal range that is not in
// taken and that free accepts. taken holds the ports other apps have
// recorded, which may belong to processes that are down at the moment, so
// they are never handed out twice; free asks the OS about the rest.
func PickPort(taken map[int]bool, free func(port int) bool) (int, error) {
	for p := manifest.InternalPortMin; p <= manifest.InternalPortMax; p++ {
		if !taken[p] && free(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port left between %d and %d for the app's process", manifest.InternalPortMin, manifest.InternalPortMax)
}
