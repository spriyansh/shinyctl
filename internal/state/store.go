package state

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/spriyansh/shinyctl/internal/atomicfile"
)

// locks holds one mutex per state-file path. "Only shinyctl-api writes
// state.json" was true at the process level but not inside it: the ticker
// goroutine, every exit-watcher goroutine, and every HTTP handler goroutine
// all Load → decide → Save the same file, and two of them interleaving
// means the second Save silently drops the first's update — an
// exit-watcher's CrashCount=3 overwritten by a ticker that loaded 2 a
// moment earlier, or apply and the ticker both deciding ActionStart.
var locks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// Lock takes the lock for path and returns the matching unlock. Hold it
// across the whole Load → decide → act → Save cycle, not just the Save —
// the point is that nobody else's decision gets made on state this caller
// is about to change. Not reentrant: a caller already holding it must not
// call anything that takes it again.
func Lock(path string) (unlock func()) {
	locks.mu.Lock()
	if locks.m == nil {
		locks.m = make(map[string]*sync.Mutex)
	}
	mu, ok := locks.m[path]
	if !ok {
		mu = &sync.Mutex{}
		locks.m[path] = mu
	}
	locks.mu.Unlock()

	mu.Lock()
	return mu.Unlock
}

// Load reads the state file at path. A missing file is an empty State,
// not an error — it's what a never-started app looks like.
func Load(path string) (State, error) {
	var state State

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, fmt.Errorf("read state: %w", err)
	}

	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("unmarshal state: %w", err)
	}

	return state, nil
}

// Save writes state to path atomically (see atomicfile.Write): a crash or
// a concurrent reader mid-write sees the old document or the new one,
// never a truncated half — which Load would otherwise reject on every
// tick from then on, permanently wedging reconciliation for that app with
// no way out short of deleting the file by hand.
func Save(path string, state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	if err := atomicfile.Write(path, data); err != nil {
		return fmt.Errorf("write state: %w", err)
	}

	return nil
}
