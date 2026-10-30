//go:build !linux && !darwin

package process

// Where neither /proc nor footprint is known, nothing is measured and ps's
// resident size is what status shows, as it has always been.
func platformMemory(pids []int) (map[int]int64, error) { return nil, nil }
