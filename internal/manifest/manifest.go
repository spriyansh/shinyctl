package manifest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// lookPath is exec.LookPath, as a package var so tests can fake "Rscript
// is/isn't on PATH" without depending on whether R is actually installed
// on whatever machine runs `go test`.
var lookPath = exec.LookPath

// Load reads and parses path, then makes every filesystem path in the
// result absolute. Relative paths resolve from the manifest's own
// directory, not from the current one, so a manifest names the same files
// (and the same app: app-state-file is its identity) whichever directory
// shinyctl is run from. The directory is path as given, made absolute;
// symlinks are not resolved, so a manifest reached through a link resolves
// from where the link is. A leading ~ is not expanded.
//
// This has to happen here, in the CLI process: spec.EntrypointFile and
// spec.AppStateFile cross the wire to shinyctl-api next, a long-running
// process whose own working directory is whatever it happened to inherit
// the first time it was lazily launched, and has nothing to do with where
// the manifest lives.
func Load(path string) (AppSpec, error) {
	var spec AppSpec

	data, err := os.ReadFile(path)
	if err != nil {
		return spec, fmt.Errorf("read manifest: %w", err)
	}

	if err := yaml.Unmarshal(data, &spec); err != nil {
		return spec, fmt.Errorf("parse manifest: %w", err)
	}

	manifestPath, err := filepath.Abs(path)
	if err != nil {
		return spec, fmt.Errorf("resolving manifest path: %w", err)
	}
	dir := filepath.Dir(manifestPath)

	spec.EntrypointFile = resolvePath(dir, spec.EntrypointFile)
	spec.AppStateFile = resolvePath(dir, spec.AppStateFile)

	// Default working-dir to the entrypoint's own directory — R apps
	// routinely load relative paths (data files, config) assuming they're
	// running from where app.R lives, not from shinyctl-api's arbitrary cwd.
	if spec.WorkingDir == "" {
		spec.WorkingDir = filepath.Dir(spec.EntrypointFile)
	} else {
		spec.WorkingDir = resolvePath(dir, spec.WorkingDir)
	}

	return spec, nil
}

// resolvePath makes p absolute against dir, the manifest's directory. An
// absolute p is only cleaned, as filepath.Abs did; an empty one stays empty
// so a missing field is still reported as missing.
func resolvePath(dir, p string) string {
	switch {
	case p == "":
		return ""
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	default:
		return filepath.Join(dir, p)
	}
}

// Validate checks spec for everything that can be caught before a process
// is launched: required fields, the entrypoint existing, Rscript being on
// PATH, and the value ranges of the optional fields. Runs on both sides of
// the wire — in the CLI, so a bad manifest fails before shinyctl-api is
// involved, and again in the /apply and /stop handlers.
func Validate(spec AppSpec) error {
	if spec.AppGroup == "" {
		return fmt.Errorf("app-group is required")
	}

	if spec.AppName == "" {
		return fmt.Errorf("app-name is required")
	}

	if spec.EntrypointFile == "" {
		return fmt.Errorf("entrypoint-file is required")
	}

	// Catch the two most common first-run mistakes immediately and
	// synchronously, instead of letting them surface as a silent,
	// backoff-free retry loop on shinyctl-api's side: before this check
	// existed, a typo'd path or a missing Rscript would pass Validate
	// fine, get remembered by the registry, and have the reconcile
	// ticker retry the same doomed exec() every 5s forever — the ticker
	// path never calls Validate, only apply/stop do.
	if _, err := os.Stat(spec.EntrypointFile); err != nil {
		return fmt.Errorf("entrypoint-file %q: %w", spec.EntrypointFile, err)
	}

	if _, err := lookPath("Rscript"); err != nil {
		return fmt.Errorf("Rscript not found on PATH: %w", err)
	}

	// An IP literal, not a hostname: httpuv's `host` wants an address, and
	// a daemon that restarts apps for months shouldn't be resolving names
	// on every restart. IPv6 is fine — every consumer goes through
	// net.JoinHostPort.
	if spec.Host != "" && net.ParseIP(spec.Host) == nil {
		return fmt.Errorf("host %q must be an IP address (e.g. 0.0.0.0, 127.0.0.1), not a hostname", spec.Host)
	}

	if spec.Port <= 0 || spec.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}

	// shinyctl runs exactly one instance per app: every replica would bind
	// the same spec.Port, which the OS simply refuses as a second
	// listener on it — this isn't an unimplemented feature, it's a
	// consequence of there being no proxy/load-balancer in front of the
	// app, which is deliberate: a proxy would put shinyctl-api in the
	// request path, so its death would take live apps down with it.
	// Reject rather than silently ignore, so a manifest never claims more
	// than what's actually running. Omitting the field (0) is fine — it's
	// optional precisely because 1 is its only legal value.
	if spec.Replicas < 0 || spec.Replicas > 1 {
		return fmt.Errorf("replicas must be 1 or omitted — shinyctl runs a single instance per app (no load balancer in front of it)")
	}

	switch spec.RestartPolicy {
	case "", RestartAlways, RestartOnFailure, RestartNever:
	default:
		return fmt.Errorf("restart-policy must be one of %q, %q, %q", RestartAlways, RestartOnFailure, RestartNever)
	}

	return nil
}
