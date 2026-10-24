package proxy

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeAdmin serves handler on a Unix socket and returns an Admin for it.
func fakeAdmin(t *testing.T, handler http.HandlerFunc) Admin {
	t.Helper()
	sock := filepath.Join(shortDir(t), "a.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return Admin{Socket: sock}
}

func TestBindErrorFromMessage(t *testing.T) {
	cases := []struct {
		name, raw, wantAddr, wantErr string
	}{
		{
			name:     "tcp4",
			raw:      `loading config: loading new config: http app module: start: listening on tcp4/127.0.0.1:9000: listen tcp4 127.0.0.1:9000: bind: address already in use`,
			wantAddr: "127.0.0.1:9000",
			wantErr:  "listen tcp4 127.0.0.1:9000: bind: address already in use",
		},
		{
			name:     "tcp6",
			raw:      `loading config: loading new config: http app module: start: listening on tcp6/[::1]:9000: listen tcp6 [::1]:9000: bind: address already in use`,
			wantAddr: "[::1]:9000",
			wantErr:  "listen tcp6 [::1]:9000: bind: address already in use",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":` + quote(c.raw) + `}`))
			})
			err := a.Load([]byte("{}"))
			be, ok := err.(*BindError)
			if !ok {
				t.Fatalf("Load error = %v (%T), want *BindError", err, err)
			}
			if be.Addr != c.wantAddr || be.Err != c.wantErr {
				t.Errorf("BindError = {%q, %q}, want {%q, %q}", be.Addr, be.Err, c.wantAddr, c.wantErr)
			}

			want := c.wantAddr + " is not available (" + c.wantErr + ") — pick a different port, or check what else is using it"
			if be.Error() != want {
				t.Errorf("Error() = %q, want %q", be.Error(), want)
			}
			if regexp.MustCompile(`(?i)caddy|app module`).MatchString(be.Error()) {
				t.Errorf("Error() leaks the engine's wording: %q", be.Error())
			}
		})
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestAdminLoad_OtherRejection(t *testing.T) {
	a := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"boom"}`))
	})
	if err := a.Load([]byte("{}")); err != errRejected {
		t.Errorf("Load error = %v, want errRejected", err)
	}
}

func TestAdmin_NotRunning(t *testing.T) {
	a := Admin{Socket: filepath.Join(shortDir(t), "none.sock")}
	if err := a.Ping(); err != ErrNotRunning {
		t.Errorf("Ping error = %v, want ErrNotRunning", err)
	}
	if err := a.Load([]byte("{}")); err != ErrNotRunning {
		t.Errorf("Load error = %v, want ErrNotRunning", err)
	}
}

// TestAdmin_OtherFailuresAreReadable: a load balancer that accepts the
// connection and then drops it is reported without the request URL and
// wrapping, since apply prints the error.
func TestAdmin_OtherFailuresAreReadable(t *testing.T) {
	a := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	err := a.Load([]byte("{}"))
	if err == nil {
		t.Fatal("Load succeeded against a socket that drops the connection")
	}
	for _, bad := range []string{"http://", "Post "} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("error %q contains %q", err, bad)
		}
	}
	if !strings.Contains(err.Error(), "shinyctl logs --api") {
		t.Errorf("error %q does not point to the log", err)
	}
}
