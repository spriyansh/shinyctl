/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/reconcile"
	"github.com/spriyansh/shinyctl/internal/state"
)

// gateway pushes the apps' settings to the load balancer and says what it
// serves. `serve` (or the tests' TestMain) sets it once, before anything
// reads it, and it is never swapped afterwards: the exit-watchers and the
// ticker read it from their own goroutines.
var gateway *proxy.Gateway

// waitForBalancer waits, up to wait, while g's load balancer is being
// started or replaced, and returns at once otherwise. It is the real Ready
// hook; it takes g so that the tests can try it on a gateway of their own
// without swapping gateway, which other goroutines read.
func waitForBalancer(g *proxy.Gateway, wait time.Duration) {
	if g != nil && g.Starting() {
		g.WaitWhileStarting(wait)
	}
}

// hooks are the few things the tests substitute (they exist for the tests,
// nothing else sets them): what the load balancer is
// serving, how an address is probed, and how long a recorded port is waited
// for. A zero field means the real one. The tests set them through an atomic
// pointer, since exit-watchers of earlier tests may still be reading.
type hooks struct {
	Served         func() []proxy.App
	Probe          func(host string, port int) error
	ProbeAlongside func(host string, port int) error
	PortWait       time.Duration
	PortEvery      time.Duration
	StartDelay     time.Duration // slept before each process is started

	// DrainSettle is reconcile.DrainSettle: how long a draining copy is kept
	// at least. Zero means the real one.
	DrainSettle time.Duration

	// Whether a copy accepts connections: the ready-watcher's probe and the
	// reconcile's own are separate so that a test can make one of them miss.
	WatchProbe   func(host string, port int) bool
	ObserveProbe func(host string, port int) bool

	// WatchExit is process.WatchExit.
	WatchExit func(spec manifest.AppSpec, app state.AppState) (<-chan struct{}, error)

	// Owner says whose the socket listening on a copy's port is (the real one
	// reads the system's tables); PortStart is where a port scan begins.
	Owner     func(pid, port int) process.Owner
	PortStart func() int

	// Sessions is what the load balancer says about its open requests per
	// copy: nil when it can't say (gateway.Sessions).
	Sessions func() map[string]int

	// Signal0 is kill(pid, 0), the ready-watcher's "is it still there".
	Signal0 func(pid int) error

	// Measure is process.ExactMemory, which an apply asks to tell which copies
	// a new max-memory is going to kill. The memory loop is given its own
	// measure as a parameter and does not read this.
	Measure measureFunc

	// Ready waits for the load balancer when it is being started or replaced
	// (up to a handover's time), before a check of what it serves: during a
	// handover it serves, but the gateway can't say so, and its own sockets
	// would be taken for another program's.
	Ready func()
}

var activeHooks atomic.Pointer[hooks]

func currentHooks() hooks {
	h := hooks{
		Served:         func() []proxy.App { return gateway.Served() },
		Ready:          func() { waitForBalancer(gateway, proxy.HandoverWait()) },
		Probe:          process.PortAvailable,
		ProbeAlongside: process.PortAvailableAlongside,
		PortWait:       time.Second,
		PortEvery:      100 * time.Millisecond,
		WatchProbe:     process.IsReady,
		ObserveProbe:   process.IsReady,
		WatchExit:      process.WatchExit,
		Sessions:       func() map[string]int { return gateway.Sessions() },
		Owner:          process.PortOwner,
		PortStart:      defaultPortStart,
		Signal0:        func(pid int) error { return syscall.Kill(pid, 0) },
		DrainSettle:    reconcile.DrainSettle,
		Measure:        process.ExactMemory,
	}
	if o := activeHooks.Load(); o != nil {
		if o.Served != nil {
			h.Served = o.Served
			h.Ready = func() {} // a test that says what is served has no load balancer to wait for
		}
		if o.Ready != nil {
			h.Ready = o.Ready
		}
		if o.Probe != nil {
			h.Probe = o.Probe
		}
		if o.ProbeAlongside != nil {
			h.ProbeAlongside = o.ProbeAlongside
		}
		if o.PortWait != 0 {
			h.PortWait = o.PortWait
		}
		if o.PortEvery != 0 {
			h.PortEvery = o.PortEvery
		}
		h.StartDelay = o.StartDelay
		if o.DrainSettle != 0 {
			h.DrainSettle = o.DrainSettle
		}
		if o.WatchProbe != nil {
			h.WatchProbe = o.WatchProbe
		}
		if o.ObserveProbe != nil {
			h.ObserveProbe = o.ObserveProbe
		}
		if o.Owner != nil {
			h.Owner = o.Owner
		}
		if o.PortStart != nil {
			h.PortStart = o.PortStart
		}
		if o.Sessions != nil {
			h.Sessions = o.Sessions
		}
		if o.WatchExit != nil {
			h.WatchExit = o.WatchExit
		}
		if o.Signal0 != nil {
			h.Signal0 = o.Signal0
		}
		if o.Measure != nil {
			h.Measure = o.Measure
		}
	}
	return h
}

// defaultPortStart is where a port scan begins; a variable so that the tests,
// which share the machine with other daemons, scan in an area of their own.
var defaultPortStart = process.PortStart

// portMu serializes choosing an internal port, so two apps starting at once
// can't pick the same one: each choice is saved to its state file before
// the next starts looking.
var portMu sync.Mutex

// newGateway builds the supervisor and gateway for a load balancer living in
// dir, started from exe, with its output going to logPath. It refuses a
// directory whose control socket path is too long to bind.
func newGateway(dir, exe, logPath string) (*proxy.Gateway, *proxy.Supervisor, error) {
	settings := proxy.SettingsFor(dir)
	if err := process.CheckSocketPath(settings.AdminSocket); err != nil {
		return nil, nil, err
	}
	var err error
	if settings.Secret, err = proxy.LoadSecret(dir); err != nil {
		return nil, nil, fmt.Errorf("the key for the load balancer's cookies: %w", err)
	}

	sup := &proxy.Supervisor{
		Settings: settings,
		Exe:      exe,
		Build:    proxy.BuildID(),
		LogPath:  logPath,
		// A load balancer that has just started or been adopted knows
		// nothing of the apps, or only what it read from copies.json: it is
		// told who serves what, from the state files, and then which apps to
		// listen for. The gateway reports what it leaves out.
		OnReady: func() {
			watchRestoredCopies()
			refreshPools()
			syncProxy("")
		},

		// A handover between builds has settled: act at once on whatever
		// waited for it, not at the next tick.
		OnHandedOver: reconcileOnce,
	}
	return proxy.NewGateway(settings, sup), sup, nil
}

// syncProxy makes the load balancer serve every managed app that has a
// process record, and returns the overall error, or else the reason the app
// at stateFile in particular is not served (nil when stateFile is "").
func syncProxy(stateFile string) error {
	appErr, err := syncProxyApp(stateFile)
	if err != nil {
		return err
	}
	return appErr
}

// syncProxyApp is syncProxy with the two kinds of failure kept apart: err is
// about the load balancer as a whole, appErr about the app at stateFile alone
// (its address is taken, by a program or by another app).
func syncProxyApp(stateFile string) (appErr, err error) {
	left, err := gateway.Sync(desiredApps())
	if err != nil {
		return nil, err
	}
	return left[stateFile], nil
}

// desiredApps lists what the load balancer should serve: the managed apps
// that have an internal port recorded. An app whose copies have all given up
// (their restart policy ends their story) and aren't running loses its port,
// as it would without the load balancer; a restarting page there would never
// end. Records are read without the state lock: they are written atomically.
func desiredApps() []proxy.App {
	var apps []proxy.App
	for _, spec := range appRegistry.All() {
		if !appRegistry.Managed(spec.AppStateFile) {
			continue
		}

		current, err := state.Load(spec.AppStateFile)
		if err != nil {
			log.Printf("proxy: %q: %v", spec.AppName, err)
			continue
		}
		var recs []state.AppState
		for _, rec := range current.Copies() {
			if rec.Copy >= 1 && rec.Port > 0 {
				recs = append(recs, rec)
			}
		}
		if len(recs) == 0 {
			continue
		}
		if allGaveUp(spec, recs) && !slices.Contains(process.AliveAll(spec, recs), true) {
			continue
		}

		apps = append(apps, proxy.App{
			Key:      spec.AppStateFile,
			Name:     spec.AppName,
			Group:    spec.AppGroup,
			Host:     spec.EffectiveHost(),
			Port:     spec.Port,
			LBPolicy: spec.EffectiveLBPolicy(),
		})
	}
	return apps
}

func allGaveUp(spec manifest.AppSpec, recs []state.AppState) bool {
	for _, rec := range recs {
		if !reconcile.GaveUp(spec, rec) {
			return false
		}
	}
	return true
}

// refreshPools rebuilds the load balancer's copies table from the state
// files, for when the daemon starts or the load balancer has just come up
// (it holds nothing, or what copies.json said). Roles are derived, not
// stored, so this gives the table the reconcile cycles would have made. A
// cycle that set a pool while this ran saw something newer, and keeps it.
func refreshPools() {
	since := gateway.PoolSeq()
	table := proxy.Table{}
	for _, spec := range appRegistry.All() {
		if !appRegistry.Managed(spec.AppStateFile) {
			continue
		}
		st, err := state.Load(spec.AppStateFile)
		if err != nil {
			log.Printf("proxy: %q: %v", spec.AppName, err)
			continue
		}
		views, _ := observe(spec, &st, observeReadOnly)
		active, warm, draining := reconcile.Roles(spec, reconcile.Input{Revision: st.Revision, Copies: views, Now: time.Now(), Settle: currentHooks().DrainSettle})
		table[spec.AppStateFile] = poolOf(reconcile.Plan{Active: active, Warm: warm, Draining: draining}, st)
	}
	gateway.ReplacePools(table, since)
}

// claimPort checks that spec's own port is there for the load balancer to
// take. The load balancer's sockets would fail a bind check, so what it
// serves is looked at first: another app that overlaps is a conflict, and an
// address the app already has is no obstacle at all. An address that differs
// from the app's own, even when it overlaps (a host widened to every
// interface), is probed the way the load balancer binds, so its own socket
// on the old address isn't counted against it but another program's is.
// Anything else is whatever process.PortAvailable says.
func claimPort(spec manifest.AppSpec) error {
	want := proxy.App{Key: spec.AppStateFile, Host: spec.EffectiveHost(), Port: spec.Port}
	h := currentHooks()
	h.Ready()
	served := h.Served()

	for _, a := range served {
		if a.Key != want.Key && a.Overlaps(want) {
			if holder, ok := appRegistry.Spec(a.Key); ok {
				a.Name, a.Group = holder.AppName, holder.AppGroup
			}
			return proxy.PortUsedBy(want, a)
		}
	}

	host := proxy.CanonicalHost(want.Host)
	for _, a := range served {
		if a.Key != want.Key {
			continue
		}
		if a.Addr() == want.Addr() {
			return nil
		}
		if a.Overlaps(want) {
			return h.ProbeAlongside(host, want.Port)
		}
	}
	return h.Probe(host, want.Port)
}

// servedApp returns what the load balancer serves for the app at key.
func servedApp(key string) (proxy.App, bool) {
	h := currentHooks()
	h.Ready()
	for _, a := range h.Served() {
		if a.Key == key {
			return a, true
		}
	}
	return proxy.App{}, false
}

// checkMove refuses a change of spec's port or host that the load balancer
// couldn't carry out, before anything is changed: the app keeps serving
// where it is. An app that isn't served, or whose address is unchanged,
// has nothing to move.
func checkMove(spec manifest.AppSpec) error {
	old, found := servedApp(spec.AppStateFile)
	if !found {
		return nil
	}

	if (proxy.App{Host: spec.EffectiveHost(), Port: spec.Port}).Addr() == old.Addr() {
		return nil
	}
	if err := claimPort(spec); err != nil {
		return fmt.Errorf("%w; %q is still on port %d", err, spec.AppName, old.Port)
	}
	return nil
}

// syncMoved brings the load balancer up to date after apply or restart, and
// makes a failed move change nothing. When the app was served before and the
// load balancer leaves it out (the new address was taken between the check
// and the reload, or the check could not see the holder, or a retry still
// finds it in the way), the previous manifest is put back and synced so the
// old listener returns, and the error is a conflict naming the port the app
// is still on. status is the HTTP status for err.
func syncMoved(spec, previous manifest.AppSpec, hadPrevious, wasServed bool) (status int, err error) {
	appErr, err := syncProxyApp(spec.AppStateFile)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if appErr == nil {
		return 0, nil
	}

	if wasServed && hadPrevious {
		if rerr := appRegistry.Remember(previous); rerr != nil {
			log.Printf("reconcile: %q: restoring the previous manifest: %v", spec.AppName, rerr)
		}
		if serr := syncProxy(""); serr != nil {
			log.Printf("reconcile: %q: restoring the previous port: %v", spec.AppName, serr)
		}
		return http.StatusConflict, fmt.Errorf("%w; %q is still on port %d", appErr, spec.AppName, previous.Port)
	}
	return http.StatusInternalServerError, appErr
}

// copyPort returns the internal port copy n of spec's app should run on. The
// one it already has is kept while it is free, so a restart needs no reload
// of the load balancer. A recorded port that looks busy is polled for about a
// second first: the likeliest holder is the process that just died, whose
// socket the OS has not released yet, and moving the port would reload the
// load balancer and cut the requests waiting for the restart. Otherwise the
// lowest free port that no other copy has recorded is picked and saved to
// the state file before returning, so a concurrent start can't pick it too.
// The caller holds spec's state lock.
func copyPort(spec manifest.AppSpec, st *state.State, n int) (int, error) {
	h := currentHooks()
	probe := func(p int) error { return process.PortAvailable(process.InternalHost, p) }

	rec, _ := st.Copy(n)
	var busy error
	hadPort := rec.Port > 0
	if hadPort {
		deadline := time.Now().Add(h.PortWait)
		for {
			if busy = probe(rec.Port); busy == nil {
				return rec.Port, nil
			}
			if !time.Now().Before(deadline) {
				break
			}
			time.Sleep(h.PortEvery)
		}
	}

	portMu.Lock()
	defer portMu.Unlock()

	port, err := process.PickPort(takenPorts(spec.AppStateFile, n), func(p int) bool { return probe(p) == nil }, h.PortStart())
	if err != nil {
		return 0, err
	}
	if hadPort {
		why := errors.Unwrap(busy)
		if why == nil {
			why = busy
		}
		log.Printf("reconcile: %q: port %d is taken (%v); moving copy %d to port %d", spec.AppName, rec.Port, why, n, port)
	}

	rec.Copy, rec.Port = n, port
	st.Put(rec)
	if err := state.Save(spec.AppStateFile, *st); err != nil {
		return 0, err
	}
	return port, nil
}

// takenPorts lists the ports copy n of the app at stateFile must not be
// given: every public port, and the internal port every other copy has
// recorded, the app's own other copies included, which may belong to a
// process that happens to be down.
func takenPorts(stateFile string, n int) map[int]bool {
	taken := map[int]bool{}
	for _, spec := range appRegistry.All() {
		taken[spec.Port] = true
		current, err := state.Load(spec.AppStateFile)
		if err != nil {
			continue
		}
		for _, rec := range current.Copies() {
			if spec.AppStateFile == stateFile && rec.Copy == n {
				continue
			}
			if rec.Copy >= 1 && rec.Port > 0 {
				taken[rec.Port] = true
			}
		}
	}
	return taken
}
