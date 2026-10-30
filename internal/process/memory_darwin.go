package process

// platformMemory measures nothing yet, so ps answers for every PID.
func platformMemory(pids []int) (map[int]int64, error) { return nil, nil }
