package process

import (
	"fmt"
	"math/rand"
	"os"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

// PickPort returns the first port in the internal range, scanning from start
// and wrapping around, that is not in taken and that free accepts. taken holds
// the ports other apps have recorded, which may belong to processes that are
// down at the moment, so they are never handed out twice; free asks the OS
// about the rest. A start outside the range is brought into it.
func PickPort(taken map[int]bool, free func(port int) bool, start int) (int, error) {
	span := manifest.InternalPortMax - manifest.InternalPortMin + 1
	first := ((start-manifest.InternalPortMin)%span + span) % span
	for i := 0; i < span; i++ {
		p := manifest.InternalPortMin + (first+i)%span
		if !taken[p] && free(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port left between %d and %d for the app's process", manifest.InternalPortMin, manifest.InternalPortMax)
}

// PortStart is where this user's port scan begins: a point of its own, from
// the user id, and a small random step. Everyone starting at the bottom of the
// range would have two users' shinyctl-api daemons, which both pick the lowest
// free port, hand the same port to two new copies at about the same time.
func PortStart() int { return portStartFor(os.Getuid(), rand.Intn(startWindow)) }

// startWindow is the width of the random step; users' starts are further
// apart than this.
const startWindow = 128

// portStartFor spreads users over the range by the golden ratio, 6180 of 10000
// per uid: neighbouring uids, which is what a Mac gives its users (501, 502),
// start far apart, 4 or 8 apart too.
func portStartFor(uid, step int) int {
	span := manifest.InternalPortMax - manifest.InternalPortMin + 1
	return manifest.InternalPortMin + (uid*6180+step)%span
}
