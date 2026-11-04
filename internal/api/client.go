package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/reconcile"
)

// unixClient builds an http.Client that dials socketPath for every
// request, regardless of the URL given to it. Unix sockets have no
// host/port, so DialContext ignores the addr net/http would normally
// derive from the URL; "http://unix/..." in the callers below is just a
// conventional placeholder to give http.Client something to parse.
func unixClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}
}

// postSpec POSTs spec as JSON to path over the Unix socket at socketPath
// and decodes an ApplyResponse back — the shape shared by /apply, /stop,
// /delete and /restart, which only differ in which endpoint they hit.
func postSpec(socketPath, path string, spec manifest.AppSpec) (ApplyResponse, error) {
	body, err := json.Marshal(spec)
	if err != nil {
		return ApplyResponse{}, err
	}

	resp, err := unixClient(socketPath).Post("http://unix"+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return ApplyResponse{}, fmt.Errorf("calling shinyctl-api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return ApplyResponse{}, fmt.Errorf("shinyctl-api: %s", bytes.TrimSpace(msg))
	}

	var out ApplyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ApplyResponse{}, err
	}

	return out, nil
}

// Options are what a request asks for beyond the spec. They travel as query
// parameters, so a shinyctl-api that doesn't know one ignores it and does the
// ordinary thing.
type Options struct {
	Now bool // stop what is replaced or no longer wanted at once instead of draining it
}

// path is endpoint with the options as its query.
func (o Options) path(endpoint string) string {
	if o.Now {
		return endpoint + "?now=1"
	}
	return endpoint
}

// Apply sends spec to shinyctl-api's /apply endpoint over the Unix socket
// at socketPath and returns its response.
func Apply(socketPath string, spec manifest.AppSpec, opts Options) (ApplyResponse, error) {
	return postSpec(socketPath, opts.path("/apply"), spec)
}

// Stop sends spec to shinyctl-api's /stop endpoint over the Unix socket at
// socketPath, asking it to stop the app and not restart it until re-applied.
func Stop(socketPath string, spec manifest.AppSpec) (ApplyResponse, error) {
	return postSpec(socketPath, "/stop", spec)
}

// Delete sends spec to shinyctl-api's /delete endpoint over the Unix socket
// at socketPath, asking it to stop the app, forget it entirely, and remove
// its state file.
func Delete(socketPath string, spec manifest.AppSpec) (ApplyResponse, error) {
	return postSpec(socketPath, "/delete", spec)
}

// Restart sends spec to shinyctl-api's /restart endpoint over the Unix
// socket at socketPath, asking it to start the app's copies again from spec
// next to the running ones, which then drain (or stop, with opts.Now).
func Restart(socketPath string, spec manifest.AppSpec, opts Options) (ApplyResponse, error) {
	return postSpec(socketPath, opts.path("/restart"), spec)
}

// ErrNoShutdown is what Shutdown returns when shinyctl-api has no /shutdown:
// it is a build from before the command.
var ErrNoShutdown = errors.New("shinyctl-api has no shutdown endpoint")

// shutdownTimeout is how long Shutdown waits for the answer. It covers a
// shinyctl-api that first waits out a handover between load balancers, then
// the apps' stops (up to 12 seconds) and the load balancer's.
const shutdownTimeout = 3 * time.Minute

// Shutdown asks shinyctl-api to stop every app and the load balancer and then
// exit. It answers before it exits.
func Shutdown(socketPath string) (ShutdownResponse, error) {
	client := unixClient(socketPath)
	client.Timeout = shutdownTimeout
	resp, err := client.Post("http://unix/shutdown", "application/json", nil)
	if err != nil {
		return ShutdownResponse{}, fmt.Errorf("calling shinyctl-api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return ShutdownResponse{}, ErrNoShutdown
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return ShutdownResponse{}, fmt.Errorf("shinyctl-api: %s", bytes.TrimSpace(msg))
	}

	var out ShutdownResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ShutdownResponse{}, err
	}
	return out, nil
}

// statusTimeout bounds a whole status request, body included. A shinyctl-api
// that accepts the connection and never answers would otherwise hang status
// (and anything that waits on it, like a cron check) forever. A variable so
// the test can shorten it.
var statusTimeout = 60 * time.Second

// statusError words an error from the status request: a timeout says that
// shinyctl-api did not answer, anything else reads as it did.
func statusError(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("shinyctl-api did not answer within %s", reconcile.FormatDuration(statusTimeout))
	}
	return nil
}

// Status fetches shinyctl-api's /status endpoint over the Unix socket at
// socketPath.
func Status(socketPath string) (StatusResponse, error) {
	client := unixClient(socketPath)
	client.Timeout = statusTimeout
	resp, err := client.Get("http://unix/status")
	if err != nil {
		if terr := statusError(err); terr != nil {
			return StatusResponse{}, terr
		}
		return StatusResponse{}, fmt.Errorf("calling shinyctl-api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return StatusResponse{}, fmt.Errorf("shinyctl-api: %s", bytes.TrimSpace(msg))
	}

	var out StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if terr := statusError(err); terr != nil {
			return StatusResponse{}, terr
		}
		return StatusResponse{}, err
	}

	return out, nil
}

// PIDHeader is the response header in which shinyctl-api names its process.
const PIDHeader = "X-Shinyctl-Pid"

// Healthy asks shinyctl-api at socketPath whether it is up: /healthz answers
// only once it is, not merely listening. It also returns the process that
// answered (0 from a shinyctl-api that doesn't say), which tells a daemon
// just started from another that was already there.
func Healthy(socketPath string) (pid int, ok bool) {
	client := unixClient(socketPath)
	client.Timeout = 500 * time.Millisecond
	resp, err := client.Get("http://unix/healthz")
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	pid, _ = strconv.Atoi(resp.Header.Get(PIDHeader))
	return pid, true
}
