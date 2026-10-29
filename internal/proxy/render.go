package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/proxy/lb"
)

// Pool and Table are the copies table's types (see package lb), so the rest
// of shinyctl names them without importing it.
type (
	Pool  = lb.Pool
	Table = lb.Table
)

// Settings says where the load balancer keeps its files and how patient it
// is with an app that is down.
type Settings struct {
	ConfigPath  string // loaded by the proxy at start; written after each successful load
	AdminSocket string // the proxy's control socket
	StorageDir  string // the engine's storage root, kept here so nothing lands in its default directories
	CopiesPath  string // the copies table, written by shinyctl-api after each update and read by the proxy at start

	// Secret is the key of the cookies that keep a visitor on one copy. It
	// must not be empty when apps are rendered; LoadSecret supplies it.
	Secret string

	// TryDuration is how long a request waits for an app that isn't
	// answering before the visitor gets the restarting page. Zero means
	// DefaultTryDuration.
	TryDuration time.Duration
}

// SettingsFor returns the settings for a load balancer living in dir
// (~/.shinyctl/proxy).
func SettingsFor(dir string) Settings {
	return Settings{
		ConfigPath:  filepath.Join(dir, "config.json"),
		AdminSocket: filepath.Join(dir, "admin.sock"),
		StorageDir:  filepath.Join(dir, "data"),
		CopiesPath:  filepath.Join(dir, "copies.json"),
	}
}

// CookieName is the name of the cookie that keeps a visitor on one copy of
// the app on port. It is unique per port, so apps on one host don't
// overwrite each other's, and says nothing about what sets it.
func CookieName(port int) string {
	return "lb_" + strconv.Itoa(port)
}

// DefaultTryDuration is long enough to cover an R process starting up, and
// short enough that a visitor to an app that is not coming back sees the
// restarting page rather than a hung tab.
const DefaultTryDuration = 15 * time.Second

// App is one app as the load balancer serves it.
type App struct {
	Key      string // the app's state file; also the server's name in the config
	Name     string // app-name, for messages
	Group    string // app-group, for messages
	Host     string // EffectiveHost
	Port     int    // the manifest's port
	LBPolicy string // the manifest's lb-policy: how a new visitor picks a copy; "" means least-conn
}

// CanonicalHost writes an IP address the way the OS and the load balancer
// name it: "::ffff:127.0.0.1" is "127.0.0.1" and "0:0:0:0:0:0:0:1" is
// "::1". A bind error names the canonical form, so addresses are compared
// and printed in it. Anything that isn't an IP address is returned as is.
func CanonicalHost(host string) string {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return host
}

// Addr is the address the load balancer listens on for a, in canonical form.
func (a App) Addr() string {
	return net.JoinHostPort(CanonicalHost(a.Host), strconv.Itoa(a.Port))
}

// family names the address family a listens in. It is "tcp4" or "tcp6",
// never plain "tcp": a plain listen on 0.0.0.0 binds dual-stack in Go,
// which would expose the app on IPv6 too. This binds what the R process
// bound before the load balancer existed, and what process.PortAvailable
// checks.
func (a App) family() string {
	if ip := net.ParseIP(a.Host); ip != nil && ip.To4() == nil {
		return "tcp6"
	}
	return "tcp4"
}

// Overlaps reports whether a and b would compete for the same port: the
// same port in the same family, on the same host or with either one on the
// family's wildcard address.
func (a App) Overlaps(b App) bool {
	if a.Port != b.Port || a.family() != b.family() {
		return false
	}
	if a.isWildcard() || b.isWildcard() {
		return true
	}
	return net.ParseIP(a.Host).Equal(net.ParseIP(b.Host))
}

func (a App) isWildcard() bool {
	ip := net.ParseIP(a.Host)
	return ip != nil && ip.IsUnspecified()
}

// listen is the config's form of a's address, with its family.
func (a App) listen() string {
	return a.family() + "/" + a.Addr()
}

// restartingPage is what a visitor sees while an app is down for longer
// than TryDuration. It reloads itself, so the tab comes back on its own.
// It is a static_response body, which expands {placeholders}, so it must
// not contain a brace.
const restartingPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="2">
<title>Restarting</title>
</head>
<body style="font-family: sans-serif; margin: 3em; color: #333">
<p>This app is restarting. This page reloads by itself every few seconds until the app is back.</p>
</body>
</html>
`

// The types below are the config's JSON, in the order Render writes it.
// Their field order fixes the output; maps are sorted by encoding/json.

type config struct {
	Admin   adminConfig   `json:"admin"`
	Logging loggingConfig `json:"logging"`
	Storage storageConfig `json:"storage"`
	Apps    *appsConfig   `json:"apps,omitempty"`
}

type adminConfig struct {
	Listen string       `json:"listen"`
	Config adminPersist `json:"config"`
}

type adminPersist struct {
	Persist bool `json:"persist"`
}

type loggingConfig struct {
	Logs map[string]logConfig `json:"logs"`
}

type logConfig struct {
	Writer  logWriter `json:"writer"`
	Level   string    `json:"level"`
	Include []string  `json:"include,omitempty"`
	Exclude []string  `json:"exclude,omitempty"`
}

type logWriter struct {
	Output string `json:"output"`
}

type storageConfig struct {
	Module string `json:"module"`
	Root   string `json:"root"`
}

type appsConfig struct {
	HTTP httpApp `json:"http"`
	TLS  tlsApp  `json:"tls"`
}

type httpApp struct {
	Servers map[string]server `json:"servers"`
}

type tlsApp struct {
	DisableStorageClean bool `json:"disable_storage_clean"`
}

type server struct {
	Listen         []string       `json:"listen"`
	Protocols      []string       `json:"protocols"`
	AutomaticHTTPS autoHTTPS      `json:"automatic_https"`
	TrustedProxies trustedProxies `json:"trusted_proxies"`
	Routes         []route        `json:"routes"`
	Errors         errorsConfig   `json:"errors"`
}

type trustedProxies struct {
	Source string   `json:"source"`
	Ranges []string `json:"ranges"`
}

type autoHTTPS struct {
	Disable bool `json:"disable"`
}

type route struct {
	Match  []match `json:"match,omitempty"`
	Handle []any   `json:"handle"`
}

type match struct {
	Vars map[string][]string `json:"vars"`
}

type errorsConfig struct {
	Routes []route `json:"routes"`
}

// inFlightHandler counts the requests being answered (see lb.InFlight).
type inFlightHandler struct {
	Handler string `json:"handler"`
}

type headersHandler struct {
	Handler  string    `json:"handler"`
	Response headerOps `json:"response"`
}

type headerOps struct {
	Delete   []string `json:"delete"`
	Deferred bool     `json:"deferred,omitempty"`
}

type reverseProxy struct {
	Handler          string           `json:"handler"`
	DynamicUpstreams dynamicUpstreams `json:"dynamic_upstreams"`
	LoadBalancing    loadBalancing    `json:"load_balancing"`
	HealthChecks     healthChecks     `json:"health_checks"`
	Headers          requestHeaders   `json:"headers"`
	StreamCloseDelay string           `json:"stream_close_delay"`
}

// dynamicUpstreams names the source of the app's copies: the copies table
// (see package lb), under the app's key.
type dynamicUpstreams struct {
	Source string `json:"source"`
	App    string `json:"app"`
}

type loadBalancing struct {
	SelectionPolicy selectionPolicy `json:"selection_policy"`
	TryDuration     string          `json:"try_duration"`
	TryInterval     string          `json:"try_interval"`
}

// selectionPolicy keeps a visitor on one copy; Fallback picks among the
// copies of a role for a visitor who has none.
type selectionPolicy struct {
	Policy   string     `json:"policy"`
	App      string     `json:"app"`
	Cookie   string     `json:"cookie"`
	Secret   string     `json:"secret"`
	Fallback policyName `json:"fallback"`
}

type policyName struct {
	Policy string `json:"policy"`
}

type healthChecks struct {
	Passive passiveChecks `json:"passive"`
}

type passiveChecks struct {
	FailDuration string `json:"fail_duration"`
	MaxFails     int    `json:"max_fails"`
}

type requestHeaders struct {
	Request struct {
		Delete []string `json:"delete"`
	} `json:"request"`
}

type staticResponse struct {
	Handler    string              `json:"handler"`
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers"`
	Body       string              `json:"body"`
}

// Render returns the load balancer's whole config for apps: one server per
// app, listening on the app's own port and forwarding to its copies. It is
// pure and deterministic, so a changed config is a changed document and an
// unchanged one can be told apart from it. With no apps it returns the
// bootstrap, which has no "apps" key at all.
//
// What each part is for:
//   - stream_close_delay keeps open WebSockets (the Shiny sessions) alive
//     across a config reload, which would otherwise close them.
//   - the copies come from the copies table (dynamic_upstreams), not from
//     the settings, so a copy that dies, a spare that takes over and a
//     scale change take effect without a reload; a reload binds every
//     listener again and drops the connections that arrive meanwhile.
//   - the selection policy keeps each visitor on one copy with a cookie
//     (see CookieName), and sends new visitors to the active copies; a warm
//     copy takes visitors only as the stand-in for an active copy that is
//     down, and a visitor of a copy that has just left the table goes to
//     the copy that took its place.
//   - try_duration and try_interval make a request wait for an app that is
//     restarting.
//   - passive health checks are a backstop for a copy that died before the
//     table says so: one failed dial marks it down for two seconds and the
//     visitor moves on within a try_interval. The policy dials a copy that
//     is marked down when nothing else is up, so a lone copy that restarts
//     is not held back for those two seconds.
//   - trusted_proxies makes the load balancer extend the X-Forwarded-*
//     headers of a front proxy on this machine (nginx terminating TLS, say)
//     instead of replacing them, so the app still sees the visitor's
//     address and scheme. Only loopback is trusted: anyone else's are
//     overwritten, as before.
//   - the deferred headers handler removes Server and Via from the response
//     (the engine sets both after the route's own header operations have
//     run, and again on a WebSocket's 101), and the request-side delete
//     keeps Via from reaching the app.
//   - the errors routes strip the same headers, which the errors chain
//     doesn't pass through the deferred handler for, and turn 502 and 503
//     into a restarting page that reloads itself.
func Render(s Settings, apps []App) ([]byte, error) {
	if len(apps) > 0 && s.Secret == "" {
		return nil, errors.New("rendering settings: no secret")
	}
	try := s.TryDuration
	if try <= 0 {
		try = DefaultTryDuration
	}

	cfg := config{
		Admin: adminConfig{
			Listen: "unix/" + s.AdminSocket + "|0600",
			Config: adminPersist{Persist: false},
		},
		Logging: loggingConfig{Logs: map[string]logConfig{
			"admin_quiet": {Writer: logWriter{plainLogWriterName}, Level: "ERROR", Include: []string{"admin"}},
			"default":     {Writer: logWriter{plainLogWriterName}, Level: "WARN", Exclude: []string{"admin"}},
		}},
		Storage: storageConfig{Module: "file_system", Root: s.StorageDir},
	}

	if len(apps) > 0 {
		servers := make(map[string]server, len(apps))
		for _, a := range apps {
			fallback, err := fallbackPolicy(a.LBPolicy)
			if err != nil {
				return nil, fmt.Errorf("rendering settings: app %q: %w", a.Name, err)
			}
			servers[a.Key] = renderServer(a, s.Secret, fallback, try)
		}
		cfg.Apps = &appsConfig{
			HTTP: httpApp{Servers: servers},
			TLS:  tlsApp{DisableStorageClean: true},
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // the restarting page is HTML
	enc.SetIndent("", "  ")
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// fallbackPolicy is the engine's name for a manifest's lb-policy.
func fallbackPolicy(lbPolicy string) (string, error) {
	switch lbPolicy {
	case "", manifest.LBLeastConn:
		return "least_conn", nil
	case manifest.LBRoundRobin:
		return "round_robin", nil
	case manifest.LBRandom, manifest.LBFirst:
		return lbPolicy, nil
	}
	return "", fmt.Errorf("unknown lb-policy %q", lbPolicy)
}

func renderServer(a App, secret, fallback string, try time.Duration) server {
	var rp reverseProxy
	rp.Handler = "reverse_proxy"
	rp.DynamicUpstreams = dynamicUpstreams{Source: "shinyctl_copies", App: a.Key}
	rp.LoadBalancing = loadBalancing{
		SelectionPolicy: selectionPolicy{
			Policy:   "shinyctl_sticky",
			App:      a.Key,
			Cookie:   CookieName(a.Port),
			Secret:   secret,
			Fallback: policyName{Policy: fallback},
		},
		TryDuration: try.String(),
		TryInterval: "250ms",
	}
	rp.HealthChecks = healthChecks{Passive: passiveChecks{FailDuration: "2s", MaxFails: 1}}
	rp.Headers.Request.Delete = []string{"Via"}
	rp.StreamCloseDelay = "12h"

	return server{
		Listen:         []string{a.listen()},
		Protocols:      []string{"h1"},
		AutomaticHTTPS: autoHTTPS{Disable: true},
		TrustedProxies: trustedProxies{Source: "static", Ranges: []string{"127.0.0.0/8", "::1/128"}},
		Routes: []route{{Handle: []any{
			inFlightHandler{Handler: "shinyctl_inflight"},
			headersHandler{Handler: "headers", Response: headerOps{Delete: []string{"Server", "Via"}, Deferred: true}},
			rp,
		}}},
		Errors: errorsConfig{Routes: []route{
			{Handle: []any{
				headersHandler{Handler: "headers", Response: headerOps{Delete: []string{"Server", "Via"}}},
			}},
			{
				Match: []match{{Vars: map[string][]string{"{http.error.status_code}": {"502", "503"}}}},
				Handle: []any{staticResponse{
					Handler:    "static_response",
					StatusCode: 503,
					Headers: map[string][]string{
						"Cache-Control": {"no-store"},
						"Content-Type":  {"text/html; charset=utf-8"},
						"Retry-After":   {"2"},
					},
					Body: restartingPage,
				}},
			},
		}},
	}
}
