package manifest

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
