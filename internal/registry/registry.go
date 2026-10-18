package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/spriyansh/shinyctl/internal/atomicfile"
	"github.com/spriyansh/shinyctl/internal/manifest"
)

// Registry remembers the most recently applied spec for each app, keyed by
// its state file path, so the background reconcile loop has something to
// check even when no client is actively asking about it right now. It also
// tracks which apps have been explicitly stopped, so the reconcile loop
// knows not to bring them back up.
//
// Opened with a path, it writes through to disk on every change and
// reloads from there on the next Open — so a restarted shinyctl-api
// resumes watching everything the previous one was watching, instead of
// coming back knowing nothing until someone re-applies. The stopped flags
// persist too: a restart must not resurrect what the user deliberately
// took down.
//
// It's accessed from at least two goroutines — an HTTP handler goroutine
// per request, and the ticker-driven reconcile loop — so every access
// needs the mutex; a map is not safe for concurrent use on its own.
type Registry struct {
	mu      sync.Mutex
	path    string // "" means in-memory only
	specs   map[string]manifest.AppSpec
	stopped map[string]bool
}

// entry is the on-disk shape of one remembered app.
type entry struct {
	Spec    manifest.AppSpec `json:"spec"`
	Stopped bool             `json:"stopped,omitempty"`
}

// New returns an in-memory Registry that persists nothing.
func New() *Registry {
	return &Registry{
		specs:   make(map[string]manifest.AppSpec),
		stopped: make(map[string]bool),
	}
}

// Open returns a Registry backed by the file at path, loading whatever a
// previous shinyctl-api left there. A missing file is an empty registry;
// an unreadable or malformed one is an error, not silently empty — a
// fresh start that quietly forgets every app is the exact failure this
// persistence exists to prevent.
func Open(path string) (*Registry, error) {
	r := New()
	r.path = path

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, fmt.Errorf("read registry: %w", err)
	}

	var entries []entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("unmarshal registry %s: %w", path, err)
	}

	for _, e := range entries {
		r.specs[e.Spec.AppStateFile] = e.Spec
		if e.Stopped {
			r.stopped[e.Spec.AppStateFile] = true
		}
	}

	return r, nil
}

// Remember records spec as the latest desired state for its app,
// overwriting whatever was previously remembered for the same state file.
// It also clears any prior Stop — applying an app is a declaration that
// it should be running, so it un-stops whatever was stopped before.
func (r *Registry) Remember(spec manifest.AppSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs[spec.AppStateFile] = spec
	delete(r.stopped, spec.AppStateFile)
	return r.persistLocked()
}

// All returns every spec currently remembered.
func (r *Registry) All() []manifest.AppSpec {
	r.mu.Lock()
	defer r.mu.Unlock()

	specs := make([]manifest.AppSpec, 0, len(r.specs))
	for _, spec := range r.specs {
		specs = append(specs, spec)
	}
	return specs
}

// Stop marks the app at stateFile as intentionally stopped — the
// reconcile loop must not restart it until it's Remember-ed (re-applied)
// again.
func (r *Registry) Stop(stateFile string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped[stateFile] = true
	return r.persistLocked()
}

// IsStopped reports whether the app at stateFile was explicitly stopped
// and hasn't been re-applied since.
func (r *Registry) IsStopped(stateFile string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped[stateFile]
}

// Managed reports whether the reconcile loop should be keeping the app at
// stateFile running: remembered, and not stopped. This — not IsStopped —
// is what the ticker and the exit-watchers gate on, so that an app that's
// been Forget-ten is left alone exactly like a stopped one, instead of
// being restarted by the exit-watcher of the very process delete killed.
func (r *Registry) Managed(stateFile string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, known := r.specs[stateFile]
	return known && !r.stopped[stateFile]
}

// ManagedSpec is Managed plus the remembered spec, read in one step so the
// two can't disagree. The second result is false if the app is unknown or
// stopped.
func (r *Registry) ManagedSpec(stateFile string) (manifest.AppSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	spec, known := r.specs[stateFile]
	if !known || r.stopped[stateFile] {
		return manifest.AppSpec{}, false
	}
	return spec, true
}

// Forget drops every record of the app at stateFile — its spec and any
// stopped flag — so it no longer appears in status and nothing will ever
// restart it. The next Remember (apply) starts it from scratch. Forgetting
// an unknown app is a no-op, not an error.
func (r *Registry) Forget(stateFile string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.specs, stateFile)
	delete(r.stopped, stateFile)
	return r.persistLocked()
}

// persistLocked writes the registry to disk, if it has a path. Caller
// holds mu. Entries are sorted by state file so the file is stable across
// writes — the map's iteration order is deliberately random.
func (r *Registry) persistLocked() error {
	if r.path == "" {
		return nil
	}

	entries := make([]entry, 0, len(r.specs))
	for stateFile, spec := range r.specs {
		entries = append(entries, entry{Spec: spec, Stopped: r.stopped[stateFile]})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Spec.AppStateFile < entries[j].Spec.AppStateFile
	})

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal registry: %w", err)
	}

	if err := atomicfile.Write(r.path, data); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}

	return nil
}
