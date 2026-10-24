package process

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

// ConfigDir returns ~/.shinyctl, creating it if it doesn't exist yet.
func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	dir := filepath.Join(home, ".shinyctl")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}

	return dir, nil
}

// ProxyDir returns ~/.shinyctl/proxy, where the load balancer keeps its
// settings, lock and control socket, creating it if needed.
func ProxyDir() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}

	proxyDir := filepath.Join(dir, "proxy")
	if err := os.MkdirAll(proxyDir, 0700); err != nil {
		return "", err
	}

	return proxyDir, nil
}

// CheckSocketPaths checks that both Unix sockets shinyctl-api and the load
// balancer bind, api.sock and proxy/admin.sock, fit in a socket path. They
// live under the home directory, so a long one fails every start; checking
// both up front gives the reason, where the second would otherwise fail
// after the first was already bound.
func CheckSocketPaths() error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	for _, path := range []string{
		filepath.Join(dir, "api.sock"),
		filepath.Join(dir, "proxy", "admin.sock"),
	} {
		if err := CheckSocketPath(path); err != nil {
			return err
		}
	}
	return nil
}

// SocketPath returns the path to shinyctl-api's Unix domain socket,
// creating ~/.shinyctl if needed.
func SocketPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "api.sock"), nil
}

// LogPath returns the path to shinyctl-api's log file, creating
// ~/.shinyctl if needed.
func LogPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "api.log"), nil
}

// RegistryPath returns the path shinyctl-api persists its registry of
// applied apps to, creating ~/.shinyctl if needed.
func RegistryPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "registry.json"), nil
}

// AppLogPath returns the path to copy 1's log file, which is also the one
// an app with a single copy has: see CopyLogPath.
func AppLogPath(spec manifest.AppSpec) (string, error) {
	return CopyLogPath(spec, 1)
}

// CopyLogPath returns the path to the log file of copy n of an app,
// ~/.shinyctl/logs/<app-group>-<app-name>.log for copy 1 (the name an app
// had before it could have copies) and <app-group>-<app-name>@<n>.log for
// the others. The name comes from group and name so two apps under the same
// shinyctl-api never share one; app names may not contain "@", so a copy's
// file can't be taken for another app's. Creates ~/.shinyctl/logs if needed.
func CopyLogPath(spec manifest.AppSpec, n int) (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}

	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0700); err != nil {
		return "", err
	}

	return filepath.Join(logsDir, copyLogName(spec, n)), nil
}

func copyLogName(spec manifest.AppSpec, n int) string {
	if n <= 1 {
		return fmt.Sprintf("%s-%s.log", spec.AppGroup, spec.AppName)
	}
	return fmt.Sprintf("%s-%s@%d.log", spec.AppGroup, spec.AppName, n)
}

// CopyLog is the log file of one copy.
type CopyLog struct {
	Copy int
	Path string
}

// AppLogPaths lists the log files an app's copies have, sorted by copy. The
// names are matched by string comparison, not a pattern: an app name may
// hold characters a pattern would read as its own.
func AppLogPaths(spec manifest.AppSpec) ([]CopyLog, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	logsDir := filepath.Join(dir, "logs")

	entries, err := os.ReadDir(logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	first := copyLogName(spec, 1)
	prefix, suffix := fmt.Sprintf("%s-%s@", spec.AppGroup, spec.AppName), ".log"

	var logs []CopyLog
	for _, e := range entries {
		name := e.Name()
		switch {
		case name == first:
			logs = append(logs, CopyLog{1, filepath.Join(logsDir, name)})
		case strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) && len(name) > len(prefix)+len(suffix):
			digits := name[len(prefix) : len(name)-len(suffix)]
			// Only the form shinyctl writes: no sign, no leading zero, and
			// never 1, which has the plain name.
			if n, err := strconv.Atoi(digits); err == nil && n >= 2 && strconv.Itoa(n) == digits {
				logs = append(logs, CopyLog{n, filepath.Join(logsDir, name)})
			}
		}
	}
	slices.SortFunc(logs, func(a, b CopyLog) int { return a.Copy - b.Copy })
	return logs, nil
}
