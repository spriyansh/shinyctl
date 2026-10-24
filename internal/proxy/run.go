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
)

// stopTimeout bounds the shutdown. Stopping doesn't wait for open
// connections, so this is only a safety net.
const stopTimeout = 5 * time.Second

// Run is the proxy process: it takes the lock, loads the config at
// configPath, and serves until SIGTERM or SIGINT. It never touches HOME:
// the engine's own autosave is off, its TLS app is told not to keep
// housekeeping files, and its storage root is inside the proxy directory
// (all in the rendered config).
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
func Run(configPath string) error {
	lock, err := acquireLock(LockPath(configPath), lockInfo{PID: os.Getpid(), Build: BuildID()})
	if err != nil {
		return err
	}
	defer lock.Close()

	dir := filepath.Dir(configPath)
	var loadErr error
	if cfg, err := os.ReadFile(configPath); err != nil {
		loadErr = err
	} else if cfg, err = withOwnSettings(cfg, dir); err != nil {
		loadErr = err
	} else {
		loadErr = caddy.Load(cfg, true)
	}
	if loadErr != nil {
		log.Printf("proxy: could not load %s: %s; serving no apps until shinyctl-api sends new settings", configPath, describeLoadError(loadErr))
		boot, err := Render(SettingsFor(dir), nil)
		if err != nil {
			return fmt.Errorf("proxy: %w", err)
		}
		if err := caddy.Load(boot, true); err != nil {
			return fmt.Errorf("proxy: starting without apps: %w", err)
		}
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs

	stopped := make(chan struct{})
	go func() {
		caddy.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(stopTimeout):
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
