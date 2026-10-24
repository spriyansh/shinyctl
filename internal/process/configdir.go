package process

import (
	"fmt"
	"os"
	"path/filepath"

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

// AppLogPath returns the path to one app's own log file,
// ~/.shinyctl/logs/<app-group>-<app-name>.log — derived from group and
// name so two apps under the same shinyctl-api never share one. Creates
// ~/.shinyctl/logs if needed.
func AppLogPath(spec manifest.AppSpec) (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}

	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0700); err != nil {
		return "", err
	}

	filename := fmt.Sprintf("%s-%s.log", spec.AppGroup, spec.AppName)
	return filepath.Join(logsDir, filename), nil
}
