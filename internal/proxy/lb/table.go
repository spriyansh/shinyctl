package lb

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"slices"
	"sync/atomic"
)

// Pool is one app's copies by role, as "127.0.0.1:<port>" dial addresses,
// each list in the order the daemon ranks them.
type Pool struct {
	Active []string `json:"active,omitempty"` // take new visitors
	Warm   []string `json:"warm,omitempty"`   // ready spares: take visitors only when no active copy is up
	// Draining copies keep the visitors already pinned to them and get no
	// new ones. Nothing sets it yet.
	Draining []string `json:"draining,omitempty"`
}

// Table is every app's pool, keyed by the app's key: its state file, which
// is also the server's name in the settings.
type Table map[string]Pool

// Equal reports whether t and u have the same apps with the same lists in
// the same order. A nil list and an empty one are equal.
func (t Table) Equal(u Table) bool {
	if len(t) != len(u) {
		return false
	}
	for key, p := range t {
		q, ok := u[key]
		if !ok || !slices.Equal(p.Active, q.Active) || !slices.Equal(p.Warm, q.Warm) || !slices.Equal(p.Draining, q.Draining) {
			return false
		}
	}
	return true
}

// Clone returns a copy of t that shares nothing with it.
func (t Table) Clone() Table {
	out := make(Table, len(t))
	for key, p := range t {
		out[key] = Pool{Active: slices.Clone(p.Active), Warm: slices.Clone(p.Warm), Draining: slices.Clone(p.Draining)}
	}
	return out
}

type role uint8

const (
	roleNone role = iota
	roleActive
	roleWarm
	roleDraining
)

// tiers is the order roles are tried in when a visitor needs a copy.
var tiers = [...]role{roleActive, roleWarm, roleDraining}

// snapshot is a table with what the readers need already worked out. It is
// never changed after it is built.
type snapshot struct {
	table Table
	roles map[string]map[string]role // app → dial → role
	order map[string][]string        // app → dials: active, then warm, then draining, none twice
}

var current atomic.Pointer[snapshot]

func build(t Table) *snapshot {
	s := &snapshot{table: t.Clone(), roles: map[string]map[string]role{}, order: map[string][]string{}}
	for key, p := range s.table {
		roles := map[string]role{}
		var order []string
		add := func(list []string, r role) {
			for _, dial := range list {
				if _, dup := roles[dial]; dup {
					continue // a dial listed twice keeps its first role
				}
				roles[dial] = r
				order = append(order, dial)
			}
		}
		add(p.Active, roleActive)
		add(p.Warm, roleWarm)
		add(p.Draining, roleDraining)
		s.roles[key], s.order[key] = roles, order
	}
	return s
}

func load() *snapshot {
	if s := current.Load(); s != nil {
		return s
	}
	return build(nil)
}

// Set replaces the whole table. Readers see either the old table or the new
// one, never a mix.
func Set(t Table) { current.Store(build(t)) }

// Current returns a copy of the table.
func Current() Table { return load().table.Clone() }

// Validate checks a table that comes from outside: every dial must be a
// host:port, and none may be listed twice in one pool.
func Validate(t Table) error {
	for key, p := range t {
		seen := map[string]bool{}
		for _, list := range [][]string{p.Active, p.Warm, p.Draining} {
			for _, dial := range list {
				if _, _, err := net.SplitHostPort(dial); err != nil {
					return fmt.Errorf("app %q: copy %q is not a host:port address", key, dial)
				}
				if seen[dial] {
					return fmt.Errorf("app %q: copy %q is listed twice", key, dial)
				}
				seen[dial] = true
			}
		}
	}
	return nil
}

// LoadFile sets the table from the file at path, which the daemon writes
// after each update so a load balancer that restarts serves at once. A
// missing file is an empty table. A file that cannot be used also sets an
// empty table, and the error says so.
func LoadFile(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		Set(nil)
		return nil
	}
	if err == nil {
		var t Table
		if err = json.Unmarshal(data, &t); err == nil {
			err = Validate(t)
		}
		if err == nil {
			Set(t)
			return nil
		}
	}
	Set(nil)
	return err
}
