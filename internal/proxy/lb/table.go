package lb

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Pool is one app's copies by role, as "127.0.0.1:<port>" dial addresses,
// each list in the order the daemon ranks them.
type Pool struct {
	Active []string `json:"active,omitempty"` // take new visitors
	Warm   []string `json:"warm,omitempty"`   // ready spares: take no new visitors while every active copy is up; one stands in for each active copy that is down
	// Draining copies keep the visitors pinned to them and get no new ones; a
	// visitor who loads the page again is placed anew (see pageLoad).
	// shinyctl-api lists here the copies a redeploy or a scale-down retires.
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
	roles map[string]map[string]role      // app → dial → role
	order map[string][]string             // app → dials: active, then warm, then draining, none twice
	succ  map[string]map[string]successor // app → dead dial → the copy that took its place
}

// successor is the copy that took over from an active copy that has left the
// table, and until when that is remembered.
type successor struct {
	dial  string
	until time.Time
}

// successorTTL is how long a visitor whose cookie names a copy that left the
// table is still sent to the copy that replaced it.
const successorTTL = 60 * time.Second

// now is the clock, replaceable in tests.
var now = time.Now

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

var setMu sync.Mutex

// Set replaces the whole table. Readers see either the old table or the new
// one, never a mix.
//
// It also remembers who took whose place. A visitor's cookie names a copy;
// when that copy dies the daemon's update can land while the visitor's
// request is still waiting to retry, and the table no longer lists the dead
// copy, so nothing says which copy the visitor belonged to. For each app the
// active copies that have left the table altogether are paired, in order, with
// the warm copies that became active in the same update, and a cookie that
// names a dead copy goes to its partner (see Sticky.Select). A pairing is
// kept until the dead copy is listed again, its app is gone, or successorTTL
// has passed. It lives in the snapshot, so it is replaced with the table and
// read with it; a load balancer that restarts starts with none.
func Set(t Table) {
	setMu.Lock()
	defer setMu.Unlock()
	next := build(t)
	next.succ = pairings(load(), next)
	current.Store(next)
}

// pairings is the successors of next: the still-valid ones of prev, and the
// ones that prev and next make.
func pairings(prev, next *snapshot) map[string]map[string]successor {
	t := now()
	out := map[string]map[string]successor{}
	put := func(key, dead string, s successor) {
		if out[key] == nil {
			out[key] = map[string]successor{}
		}
		out[key][dead] = s
	}
	for key, m := range prev.succ {
		for dead, s := range m {
			if _, ok := next.table[key]; ok && s.until.After(t) && next.roles[key][dead] == roleNone {
				put(key, dead, s)
			}
		}
	}
	for key, p := range next.table {
		before, ok := prev.table[key]
		if !ok {
			continue
		}
		var gone, promoted []string
		for _, dial := range before.Active {
			if next.roles[key][dial] == roleNone && !slices.Contains(gone, dial) {
				gone = append(gone, dial)
			}
		}
		for _, dial := range p.Active {
			if slices.Contains(before.Warm, dial) && !slices.Contains(promoted, dial) {
				promoted = append(promoted, dial)
			}
		}
		for i := 0; i < len(gone) && i < len(promoted); i++ {
			put(key, gone[i], successor{promoted[i], t.Add(successorTTL)})
		}
	}
	return out
}

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
