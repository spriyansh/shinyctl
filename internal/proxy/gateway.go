package proxy

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/spriyansh/shinyctl/internal/atomicfile"
	"github.com/spriyansh/shinyctl/internal/process"
)

// proc is the part of Supervisor a Gateway needs, so tests can stand in for
// it.
type proc interface {
	WaitReady(timeout time.Duration) bool
	Running() bool
	Generation() int
	PID() int
}

// Gateway pushes the set of apps to the running load balancer, and knows
// what it is serving. Changes go through a check and a reload; a rejected
// reload leaves the previous settings serving. Which copies of each app
// serve is a separate matter, the pools: they change much more often, and
// are sent as a table that replaces the previous one without a reload.
type Gateway struct {
	settings Settings
	proc     proc
	admin    Admin

	// available is process.PortAvailable, replaceable in tests;
	// availableAlongside is process.PortAvailableAlongside, for an address
	// that overlaps one the app is already served at.
	available          func(host string, port int) error
	availableAlongside func(host string, port int) error

	// readyWait is how long Sync waits for a load balancer that is not
	// running yet.
	readyWait time.Duration

	mu        sync.Mutex
	gen       int             // proc's generation that loaded and loadedCfg describe
	loaded    []App           // what the load balancer serves
	loadedCfg any             // the config it was last given, decoded; nil when unknown
	failed    map[string]bool // addresses that could not be bound, until they can
	lastLeft  map[string]string
	lastErr   string
	loads     int // successful reloads, for tests
	attempts  int // reloads sent, rejected or not, for tests

	// poolMu guards the pools, and is held across sending them, so tables
	// reach the load balancer in the order they were made. The lock order is
	// state lock, then mu, then poolMu.
	poolMu    sync.Mutex
	pools     Table             // what the load balancer should serve, by app
	poolSet   map[string]uint64 // per app, the sequence number of its last SetPool
	poolSeq   uint64
	pushed    Table // what the last successful push sent
	pushedGen int   // the proxy's generation that push went to; 0 if none
	pushes    int   // successful pushes, for tests
	lastPush  string
	badPools  map[string]bool // copies left out of the last table, to log each once
}

// NewGateway returns a Gateway for the load balancer sup supervises.
func NewGateway(s Settings, sup *Supervisor) *Gateway {
	return newGateway(s, sup)
}

func newGateway(s Settings, p proc) *Gateway {
	return &Gateway{
		settings:           s,
		proc:               p,
		admin:              Admin{Socket: s.AdminSocket},
		available:          process.PortAvailable,
		availableAlongside: process.PortAvailableAlongside,
		readyWait:          5 * time.Second,
		failed:             map[string]bool{},
		lastLeft:           map[string]string{},
		pools:              Table{},
		poolSet:            map[string]uint64{},
	}
}

// refreshLocked re-reads what the load balancer serves when it is not the
// process the last read was from: after a restart, or a first look at one
// a previous shinyctl-api started. If it can't be read, nothing is known,
// which forces the next Sync to load.
func (g *Gateway) refreshLocked() {
	gen := g.proc.Generation()
	if gen == g.gen {
		return
	}

	g.loaded, g.loadedCfg = nil, nil
	if cfg, err := g.admin.Config(); err == nil {
		if apps, err := Parse(cfg); err == nil {
			if v, err := decodeConfig(cfg); err == nil {
				g.loaded, g.loadedCfg = apps, v
			}
		}
	}
	g.gen = gen
}

// Served returns the apps the load balancer serves right now.
func (g *Gateway) Served() []App {
	if !g.proc.Running() {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refreshLocked()
	return slices.Clone(g.loaded)
}

// NotServed says why the load balancer leaves the app at key out, from the
// last Sync, or "" when it doesn't.
func (g *Gateway) NotServed(key string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastLeft[key]
}

// Serves reports whether the load balancer is running and serves the app at
// key on host:port.
func (g *Gateway) Serves(key, host string, port int) bool {
	want := App{Host: host, Port: port}
	for _, a := range g.Served() {
		if a.Key == key && a.Port == port && a.family() == want.family() && net.ParseIP(a.Host).Equal(net.ParseIP(host)) {
			return true
		}
	}
	return false
}

// Sync makes the load balancer serve apps. Apps whose address overlaps
// another's, or can't be bound, are left out and returned in left with the
// reason; they don't hold up the others. Nothing is reloaded when the
// settings it would send equal the ones it already has.
func (g *Gateway) Sync(apps []App) (left map[string]error, err error) {
	if !g.proc.WaitReady(g.readyWait) {
		return nil, ErrNotRunning
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.refreshLocked()

	keep, left := Plan(apps, g.loaded)
	defer func() { g.report(apps, keep, left, err) }()

	// An address nobody asks for any more (a move that was rolled back) is
	// forgotten, so asking for it again is tried afresh.
	for addr := range g.failed {
		if !slices.ContainsFunc(apps, func(a App) bool { return a.Addr() == addr }) {
			delete(g.failed, addr)
		}
	}

	// An address that failed to bind is tried again only once it can be:
	// every failed reload costs an error line in the log. An address that
	// overlaps the one the app is served at is probed beside that socket,
	// which would otherwise count against it.
	for _, a := range slices.Clone(keep) {
		if !g.failed[a.Addr()] {
			continue
		}
		probe := g.available
		if slices.ContainsFunc(g.loaded, func(l App) bool { return l.Key == a.Key && l.Overlaps(a) }) {
			probe = g.availableAlongside
		}
		if err := probe(CanonicalHost(a.Host), a.Port); err != nil {
			left[a.Key] = err
			keep = slices.DeleteFunc(keep, func(k App) bool { return k.Key == a.Key })
		}
	}

	for {
		cfg, err := Render(g.settings, keep)
		if err != nil {
			return left, err
		}
		next, err := decodeConfig(cfg)
		if err != nil {
			return left, err
		}
		if g.loadedCfg != nil && reflect.DeepEqual(next, g.loadedCfg) {
			g.prunePools(keep)
			return left, nil
		}

		if err := Validate(cfg); err != nil {
			log.Printf("proxy: invalid settings: %v", err)
			return left, errInvalid
		}

		// Known risk: a connection that arrives in the few milliseconds a
		// reload takes can be dropped (the engine binds a new socket for
		// every server and closes the old ones, which resets what was queued
		// on them). So reloads are kept rare: the settings are compared
		// before sending, and which copies serve, a restart and a scale
		// change go through the pools instead.
		g.attempts++
		err = g.admin.Load(cfg)
		var bind *BindError
		if errors.As(err, &bind) {
			g.failed[bind.Addr] = true
			var remaining []App
			for _, a := range keep {
				if bindsTo(a, bind.Addr) {
					left[a.Key] = bind
				} else {
					remaining = append(remaining, a)
				}
			}
			if len(remaining) < len(keep) {
				keep = remaining
				continue
			}
		}
		if err != nil {
			return left, err
		}

		g.loads++
		g.loaded, g.loadedCfg = keep, next
		for _, a := range keep {
			delete(g.failed, a.Addr())
		}
		if err := atomicfile.Write(g.settings.ConfigPath, cfg); err != nil {
			log.Printf("proxy: could not save %s: %v", g.settings.ConfigPath, err)
		}
		g.prunePools(keep)
		return left, nil
	}
}

// Reloads is the number of successful reloads so far, for tests.
func (g *Gateway) Reloads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.loads
}

// BalancerPID is the load balancer's process ID, or 0.
func (g *Gateway) BalancerPID() int {
	return g.proc.PID()
}

// SetPool records the copies that serve app key and sends the table to the
// load balancer at once. The error says the load balancer did not take it
// (it is not running, or refused it); the pool is kept and sent again with
// the next change or when the load balancer comes back, so callers log it
// and carry on.
func (g *Gateway) SetPool(key string, p Pool) error {
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	g.pools[key] = p
	g.poolSeq++
	g.poolSet[key] = g.poolSeq
	return g.pushLocked()
}

// Pool returns the copies recorded for app key.
func (g *Gateway) Pool(key string) (Pool, bool) {
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	p, ok := g.pools[key]
	return p, ok
}

// PoolSeq numbers the latest SetPool; ReplacePools takes it as the moment
// its table was made.
func (g *Gateway) PoolSeq() uint64 {
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	return g.poolSeq
}

// ReplacePools makes t the pools, for a table rebuilt from the state files:
// when the daemon starts, and when a load balancer comes up. Apps whose pool
// was set after since (a PoolSeq taken before t was built) keep it, because
// whoever set it saw something newer than t; apps not in t are dropped
// unless they are newer too.
func (g *Gateway) ReplacePools(t Table, since uint64) error {
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	for key, p := range t {
		if g.poolSet[key] <= since {
			g.pools[key] = p
		}
	}
	for key := range g.pools {
		if _, ok := t[key]; !ok && g.poolSet[key] <= since {
			delete(g.pools, key)
			delete(g.poolSet, key)
		}
	}
	return g.pushLocked()
}

// Pushes is the number of tables the load balancer has taken, for tests.
func (g *Gateway) Pushes() int {
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	return g.pushes
}

// Sessions returns the number of requests open to each copy, by its
// address, or nil when the load balancer can't say. It takes neither of the
// gateway's locks, so a status query never waits on a reload.
func (g *Gateway) Sessions() map[string]int {
	if !g.proc.Running() {
		return nil
	}
	open, err := g.admin.Upstreams()
	if err != nil {
		return nil
	}
	return open
}

// prunePools drops the pools of apps the load balancer no longer serves and
// sends the table, so a stopped or deleted app leaves nothing behind. A push
// that fails has been logged.
func (g *Gateway) prunePools(keep []App) {
	g.poolMu.Lock()
	defer g.poolMu.Unlock()
	for key := range g.pools {
		if !slices.ContainsFunc(keep, func(a App) bool { return a.Key == key }) {
			delete(g.pools, key)
			delete(g.poolSet, key)
		}
	}
	g.pushLocked()
}

// pushLocked sends the pools to the load balancer unless it already has
// them: the same table, and the same process. It never waits for a load
// balancer that is not running: a takeover must not block on a restart, and
// the supervisor's OnReady sends the table when it is back. The caller holds
// poolMu.
func (g *Gateway) pushLocked() error {
	if !g.proc.Running() {
		return ErrNotRunning
	}
	gen := g.proc.Generation()
	send := g.sendable()
	if g.pushedGen == gen && send.Equal(g.pushed) {
		return nil
	}

	if err := g.admin.PutCopies(send); err != nil {
		if msg := err.Error(); msg != g.lastPush {
			log.Printf("proxy: could not update the copies: %v", err)
			g.lastPush = msg
		}
		return err
	}
	g.lastPush = ""
	g.pushed, g.pushedGen = send.Clone(), gen
	g.pushes++

	// For a load balancer that restarts by itself: it reads this file at
	// start. The file is not the source of truth; shinyctl-api rebuilds the
	// table from the state files whenever the load balancer comes up.
	data, err := json.Marshal(send)
	if err == nil {
		err = atomicfile.Write(g.settings.CopiesPath, data)
	}
	if err != nil {
		log.Printf("proxy: could not save %s: %v", g.settings.CopiesPath, err)
	}
	return nil
}

// sendable is the pools the load balancer would accept. A copy it would
// refuse (hand-edited state naming one port for two copies, or something
// that isn't an address) is left out, the one listed first being kept, and
// logged once: one bad copy must not keep its app, or every other app, from
// reaching the load balancer, which refuses a whole table for one such
// entry. The caller holds poolMu.
func (g *Gateway) sendable() Table {
	send := make(Table, len(g.pools))
	bad := map[string]bool{}
	for key, p := range g.pools {
		seen := map[string]bool{}
		clean := func(list []string) []string {
			var out []string
			for _, dial := range list {
				var why string
				if _, _, err := net.SplitHostPort(dial); err != nil {
					why = "it is not a host:port address"
				} else if seen[dial] {
					why = "it is listed twice"
				}
				if why != "" {
					id := key + "\x00" + dial + "\x00" + why
					bad[id] = true
					if !g.badPools[id] {
						log.Printf("proxy: not sending copy %s of %s: %s", dial, key, why)
					}
					continue
				}
				seen[dial] = true
				out = append(out, dial)
			}
			return out
		}
		send[key] = Pool{Active: clean(p.Active), Warm: clean(p.Warm), Draining: clean(p.Draining)}
	}
	g.badPools = bad
	return send
}

// bindsTo reports whether a bind error for addr is about a's address. The
// error names the address canonically, which Addr does too; the overlap test
// is the fallback for anything written in a form the two don't agree on, so
// that one app's unbindable port is never left to fail every sync.
func bindsTo(a App, addr string) bool {
	if a.Addr() == addr {
		return true
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return false
	}
	return a.Overlaps(App{Host: host, Port: port})
}

// report logs what changed since the last Sync, and nothing when nothing
// did, so a standing problem is one line, not one per tick. The caller
// holds g.mu.
func (g *Gateway) report(apps, keep []App, left map[string]error, err error) {
	byKey := make(map[string]App, len(apps))
	for _, a := range apps {
		byKey[a.Key] = a
	}

	now := make(map[string]string, len(left))
	for key, why := range left {
		now[key] = why.Error()
		if g.lastLeft[key] != why.Error() {
			a := byKey[key]
			log.Printf("proxy: %q (app-group %q) is not served: %v", a.Name, a.Group, why)
		}
	}
	for key := range g.lastLeft {
		if _, still := now[key]; still {
			continue
		}
		if a, ok := byKey[key]; ok && slices.ContainsFunc(keep, func(k App) bool { return k.Key == key }) {
			log.Printf("proxy: %q (app-group %q) is served again", a.Name, a.Group)
		}
	}
	g.lastLeft = now

	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != g.lastErr && msg != "" {
		log.Printf("proxy: %v", err)
	}
	g.lastErr = msg
}
