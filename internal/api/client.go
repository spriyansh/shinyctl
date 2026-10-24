package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
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

// Apply sends spec to shinyctl-api's /apply endpoint over the Unix socket
// at socketPath and returns its response.
func Apply(socketPath string, spec manifest.AppSpec) (ApplyResponse, error) {
	return postSpec(socketPath, "/apply", spec)
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
// socket at socketPath, asking it to stop the app's process if it is
// running and start it again from spec.
func Restart(socketPath string, spec manifest.AppSpec) (ApplyResponse, error) {
	return postSpec(socketPath, "/restart", spec)
}

// Status fetches shinyctl-api's /status endpoint over the Unix socket at
// socketPath.
func Status(socketPath string) (StatusResponse, error) {
	resp, err := unixClient(socketPath).Get("http://unix/status")
	if err != nil {
		return StatusResponse{}, fmt.Errorf("calling shinyctl-api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return StatusResponse{}, fmt.Errorf("shinyctl-api: %s", bytes.TrimSpace(msg))
	}

	var out StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
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
