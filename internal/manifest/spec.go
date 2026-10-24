package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// RestartPolicy controls whether the reconcile loop restarts an app after
// it exits on its own (a deliberate `stop` always bypasses this — it's
// about unexpected exits only).
type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "always"
	RestartOnFailure RestartPolicy = "on-failure"
	RestartNever     RestartPolicy = "never"
)

// Effective returns p, defaulting an unset policy to RestartAlways — the
// behavior every manifest got before this field existed.
func (p RestartPolicy) Effective() RestartPolicy {
	if p == "" {
		return RestartAlways
	}
	return p
}

// DefaultHost is the address an app binds when the manifest doesn't say:
// every interface, so the app is reachable from the network — the lab-
// server case shinyctl exists for. Set `host: 127.0.0.1` for an app that
// should only be reachable through a reverse proxy or an SSH tunnel.
const DefaultHost = "0.0.0.0"

// InternalPortMin and InternalPortMax bound the ports shinyctl runs each
// app's R process on, on 127.0.0.1 behind the load balancer. A manifest
// may not use a port in this range, so an app's own port is never one that
// shinyctl picks for a process.
const (
	InternalPortMin = 20000
	InternalPortMax = 29999
)

type AppSpec struct {
	AppGroup       string            `yaml:"app-group" json:"app-group"`
	AppName        string            `yaml:"app-name" json:"app-name"`
	EntrypointFile string            `yaml:"entrypoint-file" json:"entrypoint-file"`
	Host           string            `yaml:"host" json:"host"`
	Port           int               `yaml:"port" json:"port"`
	Replicas       int               `yaml:"replicas" json:"replicas"`
	AppStateFile   string            `yaml:"app-state-file" json:"app-state-file"`
	RestartPolicy  RestartPolicy     `yaml:"restart-policy" json:"restart-policy"`
	WorkingDir     string            `yaml:"working-dir" json:"working-dir"`
	Env            map[string]string `yaml:"env" json:"env"`
}

// EffectiveHost returns the address the app binds, defaulting an unset
// Host to DefaultHost. An accessor rather than a default filled in by
// Load, because specs reach the process package from the wire and from
// tests too, not only from a manifest file.
func (s AppSpec) EffectiveHost() string {
	if s.Host == "" {
		return DefaultHost
	}
	return s.Host
}

// StartFields returns the manifest settings that shape the process
// process.Start launches and that the process record does not already
// hold in typed fields (app-name and entrypoint-file are compared through
// AppState.Name and EntrypointFile). Start records the map, and Decide
// compares it with the current manifest to see whether a running app was
// started from something else.
//
// port and host are not here: they say where the load balancer listens,
// not how the process runs, so changing them moves the listener and leaves
// the process alone.
//
// env is stored as a digest of the whole map rather than the map itself:
// state.json usually sits in the app's folder, where it may be backed up
// or committed, and env holds secrets. The cost is that a change can be
// reported as "env" but not as which variable.
//
// A key missing from a record means "not known", never "changed": records
// written before a key existed must not restart a running app on upgrade.
// So add keys freely, but never change how an existing key is encoded,
// or every running app would look changed.
func (s AppSpec) StartFields() map[string]string {
	return map[string]string{
		"app-group":   s.AppGroup,
		"working-dir": s.WorkingDir,
		"env":         envDigest(s.Env),
	}
}

// envDigest identifies env by content. json.Marshal sorts map keys, so the
// same variables always give the same digest, and a nil map is the same
// as an empty one.
func envDigest(env map[string]string) string {
	if env == nil {
		env = map[string]string{}
	}
	data, _ := json.Marshal(env) // a map[string]string always marshals
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
