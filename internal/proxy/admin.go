package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

var (
	// ErrNotRunning is what reaching for a load balancer that isn't
	// listening on its control socket comes to.
	ErrNotRunning = errors.New("the load balancer is not running; shinyctl-api is restarting it (see shinyctl logs --api)")

	// errRejected: the load balancer refused new settings for a reason
	// other than a port; the reason is in api.log.
	errRejected = errors.New("the load balancer rejected its new settings (see shinyctl logs --api)")

	// errInvalid: Validate failed on settings shinyctl rendered itself.
	errInvalid = errors.New("shinyctl made invalid settings for its load balancer, which is a bug (see shinyctl logs --api)")
)

// BindError is a rejected load because an address couldn't be listened on.
// Its text is process.PortAvailable's, so an app's port reads the same
// whichever check caught it.
type BindError struct {
	Addr string // host:port
	Err  string // the OS error, e.g. "listen tcp4 127.0.0.1:9000: bind: address already in use"
}

func (e *BindError) Error() string {
	return fmt.Sprintf("%s is not available (%s) — pick a different port, or check what else is using it", e.Addr, e.Err)
}

// bindFailure picks the failed listen out of the engine's wrapped error,
// e.g. "loading config: loading new config: http app module: start:
// listening on tcp4/127.0.0.1:9000: listen tcp4 127.0.0.1:9000: bind:
// address already in use".
var bindFailure = regexp.MustCompile(`listen tcp[46]? (\S+): bind: [^;]*`)

// Admin talks to the load balancer's control socket.
type Admin struct {
	Socket string
}

func (a Admin) client(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", a.Socket)
			},
			DisableKeepAlives: true,
		},
	}
}

// do sends one request; a socket nobody listens on is ErrNotRunning.
func (a Admin) do(method, path string, body []byte, timeout time.Duration) (int, []byte, error) {
	req, err := http.NewRequest(method, "http://admin"+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.client(timeout).Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return 0, nil, ErrNotRunning
		}
		// Not the request URL and its wrapping: users see this in the reply
		// to apply.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return 0, nil, fmt.Errorf("the load balancer did not answer (%w); see shinyctl logs --api", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// Ping reports whether the load balancer answers on its control socket.
func (a Admin) Ping() error {
	status, _, err := a.do(http.MethodGet, "/config/", nil, time.Second)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("the load balancer answered %d", status)
	}
	return nil
}

// Config returns the settings the load balancer is serving, exactly as
// they were last loaded.
func (a Admin) Config() ([]byte, error) {
	status, data, err := a.do(http.MethodGet, "/config/", nil, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("the load balancer answered %d", status)
	}
	return data, nil
}

// Load replaces the load balancer's settings with cfg. The old settings
// keep serving if the new ones are rejected. A port that can't be bound
// comes back as a *BindError; any other rejection is logged in full and
// returned as errRejected.
func (a Admin) Load(cfg []byte) error {
	status, data, err := a.do(http.MethodPost, "/load", cfg, 30*time.Second)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		return nil
	}

	var reply struct {
		Error string `json:"error"`
	}
	json.Unmarshal(data, &reply)
	if m := bindFailure.FindStringSubmatch(reply.Error); m != nil {
		return &BindError{Addr: m[1], Err: m[0]}
	}

	log.Printf("proxy: settings rejected: %s", bytes.TrimSpace(data))
	return errRejected
}

// decodeConfig reads cfg as generic JSON, so two configs can be compared by
// meaning instead of by bytes.
func decodeConfig(cfg []byte) (any, error) {
	var v any
	if err := json.Unmarshal(cfg, &v); err != nil {
		return nil, err
	}
	return v, nil
}
