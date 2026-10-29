package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/caddyserver/caddy/v2"

	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy/lb"
)

// handoverGrace is how long a load balancer that replaces another waits, once
// it serves, before it stops the old one: requests the old one accepted just
// before the new one was up finish. A variable so tests can change it.
var handoverGrace = 250 * time.Millisecond

// stopDelay holds the engine's stop back; only tests set it, to see that the
// wait for requests in flight is counted from the signal and not from the end
// of a slow stop.
var stopDelay time.Duration

// serveDelay holds a load balancer back between loading its settings and
// saying that it serves; only tests set it, to see what happens to the old
// load balancer in that time.
var serveDelay time.Duration

// stopTimeout bounds the shutdown: the engine's stop, and the wait for the
// requests in flight to be answered. A request still going after that is cut.
// A variable so a test can shorten it.
var stopTimeout = 5 * time.Second

// Run is the proxy process: it takes the lock, loads the config at
// configPath, and serves until SIGTERM or SIGINT. It never touches HOME:
// the engine's own autosave is off, its TLS app is told not to keep
// housekeeping files, and its storage root is inside the proxy directory
// (all in the rendered config).
//
// The copies table is read from copies.json next to the config before
// anything is loaded.
//
// With after set to the pid of the load balancer that holds the lock, this
// one is replacing it: it loads its settings and serves every port next to
// it (both listen with SO_REUSEPORT), says so on the control socket
// (lb.MarkServing), waits handoverGrace, and then stops the old one itself:
// SIGTERM, and SIGKILL if it still holds the lock after the usual grace
// period, but only while it is the lock holder and a load balancer for this
// same config. Then it takes the lock. shinyctl-api only starts it and
// watches, so the handover finishes even if shinyctl-api dies in the middle.
// If it can't load, it exits and leaves the old one serving, instead of
// serving nothing next to it; if the lock never comes, or another load
// balancer took the old one's place first, it stops serving and exits.
//
// If configPath can't be used (a port taken since it was written, invalid
// JSON, paths that name another directory after the home directory moved)
// it serves with no apps instead of exiting, so the control socket still
// answers and shinyctl-api can push a working config. That fallback is built
// from the proxy's own directory, never from the file, and so are the control
// socket, logging and storage of a file that does load: a config.json copied
// from another directory (or left by a home that was renamed) must not move
// the control socket somewhere the supervisor doesn't look, or onto another
// instance's.
func Run(configPath string, after int) error {
	lockPath := LockPath(configPath)
	info := lockInfo{PID: os.Getpid(), Build: BuildID()}
	takeover := after > 0 && lockHeld(lockPath) && waitHolder(lockPath).PID == after
	if !takeover {
		lock, err := acquireLock(lockPath, info)
		if err != nil {
			return err
		}
		defer lock.Close()
	}

	dir := filepath.Dir(configPath)

	// The copies come before the settings, so the first request already
	// finds them: shinyctl-api sends them again once it is up, but a proxy
	// that restarts on its own would otherwise answer with the restarting
	// page until then.
	copiesPath := SettingsFor(dir).CopiesPath
	if err := lb.LoadFile(copiesPath); err != nil {
		log.Printf("proxy: could not read %s: the file is not usable; serving no copies until shinyctl-api sends them", copiesPath)
	}

	var loadErr error
	if cfg, err := os.ReadFile(configPath); err != nil {
		loadErr = err
	} else if cfg, err = withOwnSettings(cfg, dir); err != nil {
		loadErr = err
	} else {
		loadErr = plainStderr(func() error { return caddy.Load(cfg, true) })
	}
	if loadErr != nil && takeover {
		return fmt.Errorf("proxy: could not load %s next to the running load balancer: %s", configPath, describeLoadError(loadErr))
	}
	if loadErr != nil {
		log.Printf("proxy: could not load %s: %s; serving no apps until shinyctl-api sends new settings", configPath, describeLoadError(loadErr))
		boot, err := Render(SettingsFor(dir), nil)
		if err != nil {
			return fmt.Errorf("proxy: %w", err)
		}
		if err := plainStderr(func() error { return caddy.Load(boot, true) }); err != nil {
			return fmt.Errorf("proxy: starting without apps: %w", err)
		}
	}

	time.Sleep(serveDelay)
	lb.MarkServing(os.Getpid())
	if takeover {
		// Requests the old one accepted just before this one was up get a
		// moment to finish; then it is stopped with SIGTERM, and the old one
		// answers the requests it is in the middle of (see the end of Run)
		// before it goes. Its control socket stays this one's.
		time.Sleep(handoverGrace)
		replaced := func() bool {
			return readHolderPID(lockPath) == after && process.HasCommandLine(after, "proxy --config "+configPath)
		}
		if _, err := process.StopIf(after, replaced); err != nil {
			log.Printf("proxy: stopping the load balancer it replaces (pid %d): %v", after, err)
		}
		lock, err := waitLock(lockPath, info, takeoverWait, after)
		if err != nil {
			caddy.Stop()
			return fmt.Errorf("proxy: the load balancer it replaces (pid %d) is still running: %w", after, err)
		}
		defer lock.Close()
		log.Printf("proxy: took over from the load balancer (pid %d)", after)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs

	// The engine closes its listeners at once on a stop, but answers the
	// requests in flight in goroutines that nothing waits for before the
	// process exits, so wait for them here (WebSockets are not counted: they
	// end now). Both waits share stopTimeout.
	deadline := time.Now().Add(stopTimeout)
	stopped := make(chan struct{})
	go func() {
		time.Sleep(stopDelay)
		caddy.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(stopTimeout):
	}
	for lb.InFlight() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// describeLoadError says why a config didn't load without repeating the
// engine's own wrapping: a port that couldn't be bound is named as it is
// elsewhere, anything else is just "unusable".
func describeLoadError(err error) string {
	if m := bindFailure.FindString(err.Error()); m != "" {
		return m
	}
	if os.IsNotExist(err) {
		return "the file is missing"
	}
	return "the settings are not usable"
}

// withOwnSettings is cfg with its admin, logging and storage replaced by the
// ones for the proxy directory dir; the apps are the file's.
func withOwnSettings(cfg []byte, dir string) ([]byte, error) {
	own, err := Render(SettingsFor(dir), nil)
	if err != nil {
		return nil, err
	}
	var mine, file map[string]json.RawMessage
	if err := json.Unmarshal(own, &mine); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(cfg, &file); err != nil {
		return nil, err
	}
	for _, key := range []string{"admin", "logging", "storage"} {
		file[key] = mine[key]
	}
	return json.Marshal(file)
}
