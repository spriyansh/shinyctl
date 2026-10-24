package proxy

import (
	"encoding/json"
	"io"
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

func TestAdmin_Upstreams_MaxPerAddress(t *testing.T) {
	// One request open to the first copy: the engine lists it twice, once from
	// its in-flight counts and once from its host table.
	a := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reverse_proxy/upstreams" {
			t.Errorf("path = %s", r.URL.Path)
		}
		io.WriteString(w, `[{"address":"127.0.0.1:20000","num_requests":1,"fails":0},`+
			`{"address":"127.0.0.1:20001","num_requests":0,"fails":0},`+
			`{"address":"127.0.0.1:20000","num_requests":1,"fails":0}]`)
	})
	got, err := a.Upstreams()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["127.0.0.1:20000"] != 1 || got["127.0.0.1:20001"] != 0 {
		t.Errorf("Upstreams = %v, want 20000:1 and 20001:0", got)
	}
}

func TestAdmin_Upstreams_Failures(t *testing.T) {
	a := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if _, err := a.Upstreams(); err == nil {
		t.Error("a 500 should be an error")
	}
	a = fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "not json") })
	if _, err := a.Upstreams(); err == nil {
		t.Error("garbage should be an error")
	}
}

func TestAdmin_PutCopies(t *testing.T) {
	var gotBody []byte
	a := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/shinyctl/copies" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
	})
	want := Table{"x": {Active: []string{"127.0.0.1:20000"}}}
	if err := a.PutCopies(want); err != nil {
		t.Fatal(err)
	}
	var sent Table
	if json.Unmarshal(gotBody, &sent) != nil || !sent.Equal(want) {
		t.Errorf("sent %s, want %v", gotBody, want)
	}

	a = fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"boom"}`)
	})
	err := a.PutCopies(want)
	if err == nil || !strings.Contains(err.Error(), "the load balancer refused the copies table") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a refusal: %v", err)
	}
}

func TestAdmin_Copies(t *testing.T) {
	a := fakeAdmin(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"x":{"active":["127.0.0.1:20000"],"warm":["127.0.0.1:20001"]}}`)
	})
	got, err := a.Copies()
	want := Table{"x": {Active: []string{"127.0.0.1:20000"}, Warm: []string{"127.0.0.1:20001"}}}
	if err != nil || !got.Equal(want) {
		t.Errorf("Copies = %v, %v; want %v", got, err, want)
	}
}
