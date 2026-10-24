package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// These tests run the load balancer inside the test process, so they share
// its global state: none may call t.Parallel.

// startInProcess loads a config for apps into this process's load balancer
// and returns its settings. The socket and storage live in a short temp dir.
func startInProcess(t *testing.T, try time.Duration, apps ...App) Settings {
	t.Helper()
	s := SettingsFor(shortDir(t))
	s.TryDuration = try
	cfg, err := Render(s, apps)
	if err != nil {
		t.Fatal(err)
	}
	if err := caddy.Load(cfg, true); err != nil {
		t.Fatalf("loading the config: %v", err)
	}
	t.Cleanup(func() { caddy.Stop() })
	return s
}

func appURL(a App) string { return "http://" + a.Addr() + "/" }

func TestServe_ForwardsAndHidesHeaders(t *testing.T) {
	var gotVia, gotXFF string
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVia, gotXFF = r.Header.Get("Via"), r.Header.Get("X-Forwarded-For")
		io.WriteString(w, "from the app")
	}))
	app := upstreamApp("a", freePort(t), up)
	startInProcess(t, 0, app)

	// A client-supplied Via must not reach the app either.
	req, _ := http.NewRequest("GET", appURL(app), nil)
	req.Header.Set("Via", "1.1 somewhere")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 || string(body) != "from the app" {
		t.Errorf("response = %d %q, want 200 from the app", resp.StatusCode, body)
	}
	for _, h := range []string{"Server", "Via"} {
		if v := resp.Header.Values(h); len(v) > 0 {
			t.Errorf("response has %s: %v", h, v)
		}
	}
	if gotVia != "" {
		t.Errorf("the app saw Via: %q", gotVia)
	}
	if gotXFF == "" {
		t.Error("the app saw no X-Forwarded-For")
	}
}

// upgradeHandler answers an Upgrade request with a 101 and then echoes
// whatever it is sent.
var upgradeHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		io.WriteString(w, "plain")
		return
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	rw.Flush()
	io.Copy(conn, rw)
})

// upgrade opens a connection to addr and upgrades it, returning the
// connection, its reader, and the response's header block.
func upgrade(t *testing.T, addr string) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", addr)

	r := bufio.NewReader(conn)
	var head strings.Builder
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the upgrade response: %v (got %q)", err, head.String())
		}
		head.WriteString(line)
		if line == "\r\n" {
			break
		}
	}
	conn.SetReadDeadline(time.Time{})
	return conn, r, head.String()
}

// echoes sends msg on conn and expects it back.
func echoes(t *testing.T, conn net.Conn, r *bufio.Reader, msg string) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatalf("writing to the stream: %v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != msg {
		t.Fatalf("stream echoed %q, %v, want %q", buf, err, msg)
	}
}

func TestServe_WebSocket(t *testing.T) {
	app := upstreamApp("a", freePort(t), startUpstream(t, upgradeHandler))
	startInProcess(t, 0, app)

	conn, r, head := upgrade(t, app.Addr())
	if !strings.HasPrefix(head, "HTTP/1.1 101") {
		t.Fatalf("response head = %q, want a 101", head)
	}
	for _, h := range []string{"Server:", "Via:"} {
		if regexp.MustCompile(`(?im)^` + h).MatchString(head) {
			t.Errorf("the 101 carries %s\n%s", h, head)
		}
	}
	echoes(t, conn, r, "ping")
}

func TestServe_StreamSurvivesReload(t *testing.T) {
	a := upstreamApp("a", freePort(t), startUpstream(t, upgradeHandler))
	s := startInProcess(t, 0, a)

	conn, r, head := upgrade(t, a.Addr())
	if !strings.HasPrefix(head, "HTTP/1.1 101") {
		t.Fatalf("response head = %q, want a 101", head)
	}
	echoes(t, conn, r, "before")

	b := upstreamApp("b", freePort(t), startUpstream(t, upgradeHandler))
	cfg, err := Render(s, []App{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if err := (Admin{Socket: s.AdminSocket}).Load(cfg); err != nil {
		t.Fatal(err)
	}

	echoes(t, conn, r, "after")
}

func TestServe_WaitsOutRestart(t *testing.T) {
	port := freePort(t)
	app := upstreamApp("a", freePort(t), fmt.Sprintf("127.0.0.1:%d", port))
	startInProcess(t, 0, app)

	go func() {
		time.Sleep(600 * time.Millisecond)
		serveOn(t, port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "back") }))
	}()

	start := time.Now()
	status, body, _ := get(t, appURL(app))
	if status != 200 || body != "back" {
		t.Errorf("response = %d %q, want 200 back", status, body)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %s, want under 5s", took)
	}
}

var productName = regexp.MustCompile(`(?i)caddy`)

func TestServe_RestartingPage(t *testing.T) {
	app := upstreamApp("a", freePort(t), fmt.Sprintf("127.0.0.1:%d", freePort(t)))
	startInProcess(t, time.Second, app)

	status, body, h := get(t, appURL(app))
	if status != 503 {
		t.Errorf("status = %d, want 503", status)
	}
	if got := h.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := h.Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2", got)
	}
	if got := h.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	for _, name := range []string{"Server", "Via"} {
		if v := h.Values(name); len(v) > 0 {
			t.Errorf("response has %s: %v", name, v)
		}
	}
	if !strings.Contains(body, "This app is restarting") {
		t.Errorf("body = %q, want the restarting page", body)
	}
	if productName.MatchString(body) || productName.MatchString(fmt.Sprint(h)) {
		t.Errorf("the response names the software:\n%v\n%s", h, body)
	}
}

// TestServe_ReloadDuringRetryShowsPage: a reload cancels requests that are
// waiting for a copy, and they come back as 502, which the page must cover.
func TestServe_ReloadDuringRetryShowsPage(t *testing.T) {
	app := upstreamApp("a", freePort(t), fmt.Sprintf("127.0.0.1:%d", freePort(t)))
	s := startInProcess(t, 15*time.Second, app)

	type result struct {
		status int
		body   string
	}
	done := make(chan result, 1)
	go func() {
		status, body, _ := get(t, appURL(app))
		done <- result{status, body}
	}()

	time.Sleep(300 * time.Millisecond)
	other := upstreamApp("b", freePort(t), startUpstream(t, upgradeHandler))
	cfg, err := Render(s, []App{app, other})
	if err != nil {
		t.Fatal(err)
	}
	if err := (Admin{Socket: s.AdminSocket}).Load(cfg); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.status != 503 || !strings.Contains(r.body, "This app is restarting") {
			t.Errorf("response = %d %q, want the 503 page", r.status, r.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting request did not return within 2s of the reload")
	}
}

func TestServe_RejectedConfigKeepsOld(t *testing.T) {
	a := upstreamApp("a", freePort(t), startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "a") })))
	s := startInProcess(t, 0, a)

	taken := freePort(t)
	hold(t, taken)
	b := upstreamApp("b", taken, a.Upstreams[0])
	cfg, err := Render(s, []App{a, b})
	if err != nil {
		t.Fatal(err)
	}

	err = (Admin{Socket: s.AdminSocket}).Load(cfg)
	be, ok := err.(*BindError)
	if !ok {
		t.Fatalf("Load error = %v (%T), want *BindError", err, err)
	}
	if want := fmt.Sprintf("127.0.0.1:%d", taken); be.Addr != want {
		t.Errorf("BindError.Addr = %q, want %q", be.Addr, want)
	}
	if status, body, _ := get(t, appURL(a)); status != 200 || body != "a" {
		t.Errorf("after the rejected load, app a answers %d %q, want 200 a", status, body)
	}
}

func TestServe_HostChangeOnSamePort(t *testing.T) {
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	app := upstreamApp("a", freePort(t), up)
	app.Host = "0.0.0.0"
	s := startInProcess(t, 0, app)

	app.Host = "127.0.0.1"
	cfg, err := Render(s, []App{app})
	if err != nil {
		t.Fatal(err)
	}
	if err := (Admin{Socket: s.AdminSocket}).Load(cfg); err != nil {
		t.Fatal(err)
	}
	if status, body, _ := get(t, appURL(app)); status != 200 || body != "ok" {
		t.Errorf("after the host change, %s answers %d %q, want 200 ok", app.Addr(), status, body)
	}
}

// TestServe_ConfigRoundTrip: the engine hands back the config as posted, so
// a freshly started shinyctl-api can compare it with what it would render
// and skip a reload.
func TestServe_ConfigRoundTrip(t *testing.T) {
	a := upstreamApp("a", freePort(t), "127.0.0.1:20000")
	b := upstreamApp("b", freePort(t), "127.0.0.1:20001")
	s := startInProcess(t, 0, a, b)

	want, err := Render(s, []App{a, b})
	if err != nil {
		t.Fatal(err)
	}
	got, err := (Admin{Socket: s.AdminSocket}).Config()
	if err != nil {
		t.Fatal(err)
	}
	wantV, _ := decodeConfig(want)
	gotV, _ := decodeConfig(got)
	if !reflect.DeepEqual(wantV, gotV) {
		t.Errorf("the config read back differs from the one rendered:\nrendered: %s\nread back: %s", want, got)
	}
}

// TestServe_ForwardsFrontProxyHeaders: a front proxy on this machine (nginx
// terminating TLS) sends the visitor's address and scheme. They are extended,
// not replaced, so the app still sees them.
func TestServe_ForwardsFrontProxyHeaders(t *testing.T) {
	var gotXFF, gotProto, gotHost string
	up := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXFF = r.Header.Get("X-Forwarded-For")
		gotProto = r.Header.Get("X-Forwarded-Proto")
		gotHost = r.Header.Get("X-Forwarded-Host")
	}))
	app := upstreamApp("a", freePort(t), up)
	startInProcess(t, 0, app)

	req, _ := http.NewRequest("GET", appURL(app), nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "lab.example.org")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotXFF != "203.0.113.7, 127.0.0.1" {
		t.Errorf("X-Forwarded-For = %q, want the visitor first and the front proxy appended", gotXFF)
	}
	if gotProto != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", gotProto)
	}
	if gotHost != "lab.example.org" {
		t.Errorf("X-Forwarded-Host = %q, want lab.example.org", gotHost)
	}
}
