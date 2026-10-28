package lb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/caddyconfig"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp"
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

// These tests run the real engine in this process, with the modules in
// this package and a hand-written config (the package that renders the real
// one imports this one). One engine runs at a time, so none of them runs in
// parallel.

// upstream is an HTTP server on a fixed address that can be stopped and
// started again on it, standing in for one copy.
type upstream struct {
	addr string
	body string
	srv  *http.Server
}

func newUpstream(t *testing.T, body string) *upstream {
	t.Helper()
	u := &upstream{body: body}
	u.listen(t, "127.0.0.1:0")
	t.Cleanup(u.kill)
	return u
}

func (u *upstream) listen(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	u.addr = ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		sc := bufio.NewScanner(rw)
		for sc.Scan() {
			fmt.Fprintf(rw, "%s:%s\n", u.body, sc.Text())
			rw.Flush()
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, u.body) })
	u.srv = &http.Server{Handler: mux}
	go u.srv.Serve(ln)
}

func (u *upstream) kill() { u.srv.Close() }

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// engineApp is one app of the hand-written config.
type engineApp struct {
	key    string
	port   int
	cookie string
	tryInt string
	policy string // the fallback policy; least_conn when empty
}

func engineConfig(sock string, apps []engineApp) []byte {
	servers := map[string]any{}
	for _, a := range apps {
		tryInt := a.tryInt
		if tryInt == "" {
			tryInt = "250ms"
		}
		policy := a.policy
		if policy == "" {
			policy = "least_conn"
		}
		rp := map[string]any{
			"handler":           "reverse_proxy",
			"dynamic_upstreams": map[string]any{"source": "shinyctl_copies", "app": a.key},
			"load_balancing": map[string]any{
				"selection_policy": map[string]any{
					"policy": "shinyctl_sticky", "app": a.key, "cookie": a.cookie, "secret": "k",
					"fallback": map[string]any{"policy": policy},
				},
				"try_duration": "15s",
				"try_interval": tryInt,
			},
			"health_checks":      map[string]any{"passive": map[string]any{"fail_duration": "2s", "max_fails": 1}},
			"stream_close_delay": "12h",
		}
		servers[a.key] = map[string]any{
			"listen":          []string{fmt.Sprintf("tcp4/127.0.0.1:%d", a.port)},
			"protocols":       []string{"h1"},
			"automatic_https": map[string]any{"disable": true},
			"routes":          []any{map[string]any{"handle": []any{rp}}},
		}
	}
	cfg := map[string]any{
		"admin":   map[string]any{"listen": "unix/" + sock + "|0600", "config": map[string]any{"persist": false}},
		"logging": map[string]any{"logs": map[string]any{"default": map[string]any{"level": "ERROR", "writer": map[string]any{"output": "stderr"}}}},
	}
	if len(apps) > 0 {
		cfg["apps"] = map[string]any{
			"http": map[string]any{"servers": servers},
			"tls":  map[string]any{"disable_storage_clean": true},
		}
	}
	b, _ := json.Marshal(cfg)
	return b
}

// startEngine loads a config for apps and returns the control socket. The
// engine and the table are cleaned up with the test.
func startEngine(t *testing.T, apps []engineApp) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	if err := caddy.Load(engineConfig(sock, apps), true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { caddy.Stop(); Set(nil) })
	return sock
}

func adminCall(t *testing.T, sock, method, path string, body []byte) (int, []byte) {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
	req, _ := http.NewRequest(method, "http://admin"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func push(t *testing.T, sock string, tb Table) {
	t.Helper()
	b, _ := json.Marshal(tb)
	if st, body := adminCall(t, sock, "PUT", AdminPath, b); st != 200 {
		t.Fatalf("pushing the table: %d %s", st, body)
	}
}

// fetch makes one request on a new connection, with the cookie if given.
func fetch(t *testing.T, port int, cookieName, cookie string) (status int, body string, setCookies []string, took time.Duration) {
	t.Helper()
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	start := time.Now()
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatalf("GET :%d: %v", port, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header.Values("Set-Cookie"), time.Since(start)
}

func TestServe_TakeoverByPush(t *testing.T) {
	a, w := newUpstream(t, "A"), newUpstream(t, "W")
	port := freePort(t)
	Set(Table{"app": {Active: []string{a.addr}, Warm: []string{w.addr}}})
	sock := startEngine(t, []engineApp{{key: "app", port: port, cookie: "lb_x"}})

	for i := 0; i < 5; i++ {
		if _, body, _, _ := fetch(t, port, "", ""); body != "A" {
			t.Fatalf("a new visitor was served by %q while the active copy was up", body)
		}
	}

	a.kill()
	push(t, sock, Table{"app": {Active: []string{w.addr}}})
	status, body, cookies, took := fetch(t, port, "lb_x", token("k", a.addr))
	if status != 200 || body != "W" || len(cookies) != 1 || !strings.Contains(cookies[0], token("k", w.addr)) {
		t.Fatalf("pinned to the dead copy: %d %q cookies %v; want 200 from W with one cookie for W", status, body, cookies)
	}
	if took > 100*time.Millisecond {
		t.Errorf("took %v after the push, want under 100ms", took)
	}
}

func TestServe_TakeoverBackstop(t *testing.T) {
	a, w := newUpstream(t, "A"), newUpstream(t, "W")
	port := freePort(t)
	Set(Table{"app": {Active: []string{a.addr}, Warm: []string{w.addr}}})
	startEngine(t, []engineApp{{key: "app", port: port, cookie: "lb_x"}})

	a.kill() // and the table still lists it: only the passive checks know
	status, body, cookies, took := fetch(t, port, "lb_x", token("k", a.addr))
	if status != 200 || body != "W" || len(cookies) != 1 {
		t.Fatalf("pinned to the dead copy, no push: %d %q cookies %v; want 200 from W with one cookie", status, body, cookies)
	}
	if took > time.Second {
		t.Errorf("took %v, want under 1s", took)
	}
}

func TestServe_LoneCopyRestartIsNotDelayed(t *testing.T) {
	a := newUpstream(t, "A")
	port := freePort(t)
	Set(Table{"app": {Active: []string{a.addr}}})
	startEngine(t, []engineApp{{key: "app", port: port, cookie: "lb_x"}})
	if status, body, _, _ := fetch(t, port, "", ""); status != 200 || body != "A" {
		t.Fatalf("%d %q", status, body)
	}

	a.kill()
	go func() { time.Sleep(600 * time.Millisecond); a.listen(t, a.addr) }()
	status, body, _, took := fetch(t, port, "", "")
	if status != 200 || body != "A" {
		t.Fatalf("%d %q", status, body)
	}
	// Back at 600ms; without the last-resort rule the passive checks keep it
	// out until fail_duration (2s) passes.
	if took > 1200*time.Millisecond {
		t.Errorf("answered after %v, want under 1.2s", took)
	}
}

func TestServe_TablePushDropsNothing(t *testing.T) {
	a, b, c := newUpstream(t, "A"), newUpstream(t, "B"), newUpstream(t, "C")
	p1, p2 := freePort(t), freePort(t)
	Set(Table{"app1": {Active: []string{a.addr}, Warm: []string{b.addr}}, "app2": {Active: []string{c.addr}}})
	sock := startEngine(t, []engineApp{
		{key: "app1", port: p1, cookie: "lb_1"},
		{key: "app2", port: p2, cookie: "lb_2"},
	})

	var stop atomic.Bool
	var wg sync.WaitGroup
	var ok, failed atomic.Int64
	var firstErr atomic.Value
	for _, port := range []int{p1, p2} {
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(port int) {
				defer wg.Done()
				client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
				url := fmt.Sprintf("http://127.0.0.1:%d/", port)
				for !stop.Load() {
					resp, err := client.Get(url)
					if err == nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						if resp.StatusCode != 200 {
							err = fmt.Errorf("status %d", resp.StatusCode)
						}
					}
					if err != nil {
						failed.Add(1)
						firstErr.CompareAndSwap(nil, err.Error())
						continue
					}
					ok.Add(1)
				}
			}(port)
		}
	}

	time.Sleep(200 * time.Millisecond)
	begin := time.Now()
	for i := 0; i < 50; i++ {
		active, warm := a.addr, b.addr
		if i%2 == 1 {
			active, warm = b.addr, a.addr
		}
		push(t, sock, Table{"app1": {Active: []string{active}, Warm: []string{warm}}, "app2": {Active: []string{c.addr}}})
		time.Sleep(8 * time.Millisecond)
	}
	elapsed := time.Since(begin)
	stop.Store(true)
	wg.Wait()

	if elapsed < 400*time.Millisecond {
		t.Errorf("the pushes took only %v, too short to mean anything", elapsed)
	}
	if ok.Load() < 100 {
		t.Errorf("only %d requests were made", ok.Load())
	}
	if failed.Load() != 0 {
		t.Errorf("%d of %d requests failed during 50 pushes (first: %v)", failed.Load(), ok.Load()+failed.Load(), firstErr.Load())
	}
}

// upgrade opens a WebSocket-style stream through the proxy and returns the
// connection once the 101 has been read.
func upgrade(t *testing.T, port int, cookie string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	cookieLine := ""
	if cookie != "" {
		cookieLine = "Cookie: lb_x=" + cookie + "\r\n"
	}
	fmt.Fprintf(conn, "GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n%sSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", cookieLine)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	return conn, br
}

func echo(t *testing.T, conn net.Conn, br *bufio.Reader, msg string) string {
	t.Helper()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "%s\n", msg)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("echo %q: %v", msg, err)
	}
	return strings.TrimSpace(line)
}

func TestServe_WebSocketSurvivesPush(t *testing.T) {
	a, w := newUpstream(t, "A"), newUpstream(t, "W")
	port := freePort(t)
	Set(Table{"app": {Active: []string{a.addr}, Warm: []string{w.addr}}})
	sock := startEngine(t, []engineApp{{key: "app", port: port, cookie: "lb_x"}})

	conn, br := upgrade(t, port, "")
	if got := echo(t, conn, br, "one"); got != "A:one" {
		t.Fatalf("echo = %q", got)
	}
	push(t, sock, Table{"app": {Active: []string{w.addr}, Warm: []string{a.addr}}})
	if got := echo(t, conn, br, "two"); got != "A:two" {
		t.Fatalf("after demoting A: echo = %q", got)
	}
	push(t, sock, Table{"app": {Active: []string{w.addr}}})
	if got := echo(t, conn, br, "three"); got != "A:three" {
		t.Fatalf("after removing A from the table: echo = %q (an established stream is not re-routed)", got)
	}
}

func TestServe_TableSurvivesLoad(t *testing.T) {
	a, b := newUpstream(t, "A"), newUpstream(t, "B")
	p1, p2 := freePort(t), freePort(t)
	Set(Table{"app1": {Active: []string{a.addr}}, "app2": {Active: []string{b.addr}}})
	sock := startEngine(t, []engineApp{{key: "app1", port: p1, cookie: "lb_1"}})

	both := []engineApp{{key: "app1", port: p1, cookie: "lb_1"}, {key: "app2", port: p2, cookie: "lb_2"}}
	if st, body := adminCall(t, sock, "POST", "/load", engineConfig(sock, both)); st != 200 {
		t.Fatalf("load: %d %s", st, body)
	}
	if _, body, _, _ := fetch(t, p1, "", ""); body != "A" {
		t.Errorf("app1 after the load: %q", body)
	}
	if _, body, _, _ := fetch(t, p2, "", ""); body != "B" {
		t.Errorf("app2 after the load: %q", body)
	}
}

func TestServe_DrainingKeepsPinned(t *testing.T) {
	a, b := newUpstream(t, "A"), newUpstream(t, "B")
	port := freePort(t)
	Set(Table{"app": {Active: []string{b.addr}, Draining: []string{a.addr}}})
	startEngine(t, []engineApp{{key: "app", port: port, cookie: "lb_x"}})

	for i := 0; i < 10; i++ {
		if _, body, _, _ := fetch(t, port, "", ""); body != "B" {
			t.Fatalf("a new visitor was served by %q, a draining copy", body)
		}
		if _, body, cookies, _ := fetch(t, port, "lb_x", token("k", a.addr)); body != "A" || len(cookies) != 0 {
			t.Fatalf("a pinned visitor was served by %q with cookies %v; want A, none", body, cookies)
		}
	}
}

func TestServe_BadPushIsRefused(t *testing.T) {
	a := newUpstream(t, "A")
	port := freePort(t)
	Set(Table{"app": {Active: []string{a.addr}}})
	sock := startEngine(t, []engineApp{{key: "app", port: port, cookie: "lb_x"}})

	st, _ := adminCall(t, sock, "PUT", AdminPath, []byte(`{"app":{"active":["nonsense"]}}`))
	if st != 400 {
		t.Errorf("status = %d, want 400", st)
	}
	_, body := adminCall(t, sock, "GET", AdminPath, nil)
	var got Table
	if err := json.Unmarshal(body, &got); err != nil || !got.Equal(Table{"app": {Active: []string{a.addr}}}) {
		t.Errorf("table after a refused push: %s (%v)", body, err)
	}
	if st, _ := adminCall(t, sock, "DELETE", AdminPath, nil); st != 405 {
		t.Errorf("DELETE status = %d, want 405", st)
	}
}

func TestServe_EmptyListWaitsThenRecovers(t *testing.T) {
	a := newUpstream(t, "A")
	port := freePort(t)
	Set(nil)
	sock := startEngine(t, []engineApp{{key: "app", port: port, cookie: "lb_x"}})

	go func() {
		time.Sleep(500 * time.Millisecond)
		push(t, sock, Table{"app": {Active: []string{a.addr}}})
	}()
	status, body, _, took := fetch(t, port, "", "")
	if status != 200 || body != "A" {
		t.Fatalf("%d %q", status, body)
	}
	if took < 400*time.Millisecond || took > 1500*time.Millisecond {
		t.Errorf("answered after %v; want a wait of about 500ms for the push", took)
	}
}

// Two active copies and a spare; one active copy dies and the table is not
// changed (the daemon has not noticed, or is down). The visitor of the dead
// copy lands on the spare, not on the other active copy, on the first try,
// and keeps it. New visitors (least_conn here) go to the other active copy or
// the spare, whichever has fewer connections, never to the dead one; which of
// the two is not asserted, since their counts are not in the test's hands.
func TestServe_SpareStandsInForADeadActiveCopy(t *testing.T) {
	a, b, w := newUpstream(t, "A"), newUpstream(t, "B"), newUpstream(t, "W")
	port := freePort(t)
	Set(Table{"app": {Active: []string{a.addr, b.addr}, Warm: []string{w.addr}}})
	startEngine(t, []engineApp{{key: "app", port: port, cookie: "lb_x"}})

	for i := 0; i < 5; i++ {
		if _, body, _, _ := fetch(t, port, "", ""); body == "W" {
			t.Fatalf("a new visitor was served by the spare while both active copies were up")
		}
	}

	a.kill()
	for i := 0; i < 10; i++ {
		// A new connection each time; the first request also finds out that
		// the copy is down, the later ones see it marked.
		status, body, cookies, took := fetch(t, port, "lb_x", token("k", a.addr))
		if status != 200 || body != "W" || len(cookies) != 1 || !strings.Contains(cookies[0], token("k", w.addr)) {
			t.Fatalf("visit %d, pinned to the dead copy: %d %q cookies %v; want 200 from W with one cookie for W", i, status, body, cookies)
		}
		if took > time.Second {
			t.Errorf("visit %d took %v, want under 1s", i, took)
		}
		if i == 0 {
			// Its cookie from now on is the spare's: it stays there.
			if _, body, cookies, _ := fetch(t, port, "lb_x", token("k", w.addr)); body != "W" || len(cookies) != 0 {
				t.Fatalf("a visitor placed on the spare moved: %q cookies %v", body, cookies)
			}
		}
	}
	for i := 0; i < 10; i++ {
		if status, body, _, _ := fetch(t, port, "", ""); status != 200 || body == "A" {
			t.Fatalf("new visitor: %d %q, want B or W", status, body)
		}
	}
}

// holdWS opens an upgraded connection to the copy that serves a visitor
// pinned with cookie: it counts as an open request there, like a session.
func holdWS(t *testing.T, port int, cookie string) {
	t.Helper()
	c, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nCookie: lb_x=%s\r\n\r\n", cookie)
	line, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.Contains(line, "101") {
		t.Fatalf("websocket: %q", line)
	}
	t.Cleanup(func() { c.Close() })
}

// The daemon's update (the dead copy dropped, the spare made active) lands
// while the visitor's request waits out the retry interval after its dial to
// the dead copy failed. At the retry the cookie names a copy that is no
// longer listed; the spare must still be the one that serves the visitor,
// whatever the fallback policy and the other copy's load are, every time.
func TestServe_PushDuringTheRetryWaitKeepsTheSpare(t *testing.T) {
	const rounds = 12
	for _, policy := range []string{"first", "round_robin", "random", "least_conn"} {
		for _, session := range []bool{false, true} {
			onW := 0
			for i := 0; i < rounds; i++ {
				func() {
					a, b, w := newUpstream(t, "A"), newUpstream(t, "B"), newUpstream(t, "W")
					port := freePort(t)
					Set(Table{"app": {Active: []string{a.addr, b.addr}, Warm: []string{w.addr}}})
					sock := startEngine(t, []engineApp{{key: "app", port: port, cookie: "lb_x", policy: policy}})
					defer caddy.Stop()
					if session {
						holdWS(t, port, token("k", b.addr)) // a session on the other active copy
					}
					a.kill()
					got := make(chan string, 1)
					go func() {
						_, body, _, _ := fetch(t, port, "lb_x", token("k", a.addr))
						got <- body
					}()
					time.Sleep(30 * time.Millisecond) // inside the 250ms wait of the first retry
					push(t, sock, Table{"app": {Active: []string{b.addr, w.addr}}})
					if <-got == "W" {
						onW++
					}
				}()
			}
			if onW != rounds {
				t.Errorf("%s, other copy has a session = %v: the dead copy's visitor landed on the spare %d of %d times", policy, session, onW, rounds)
			}
		}
	}
}

// A visitor pinned to a draining copy who loads the page again (the browser's
// Accept) is placed anew and gets the new version; the old page's own
// requests, its session included, stay on the draining copy.
func TestServe_DrainingPageLoadGoesToTheNewVersion(t *testing.T) {
	a, b := newUpstream(t, "A"), newUpstream(t, "B")
	port := freePort(t)
	Set(Table{"app": {Active: []string{b.addr}, Draining: []string{a.addr}}})
	startEngine(t, []engineApp{{key: "app", port: port, cookie: "lb_x"}})
	const browser = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

	get := func(accept string) (string, []string) {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
		req.AddCookie(&http.Cookie{Name: "lb_x", Value: token("k", a.addr)})
		req.Header.Set("Accept", accept)
		resp, err := (&http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body), resp.Header.Values("Set-Cookie")
	}

	body, cookies := get(browser)
	if body != "B" || len(cookies) != 1 || !strings.Contains(cookies[0], token("k", b.addr)) {
		t.Fatalf("a page load: %q, cookies %v; want B with a cookie for B", body, cookies)
	}
	if body, cookies := get("*/*"); body != "A" || len(cookies) != 0 {
		t.Fatalf("a fetch: %q, cookies %v; want A, no cookie", body, cookies)
	}
	conn, br := upgrade(t, port, token("k", a.addr))
	if got := echo(t, conn, br, "hello"); got != "A:hello" {
		t.Fatalf("the session = %q, want it on A", got)
	}
}
