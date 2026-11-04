package api

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/spriyansh/shinyctl/internal/manifest"
)

// An option travels as a query; no option, no query.
func TestOptions_NowIsAQuery(t *testing.T) {
	dir, err := os.MkdirTemp("", "api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var seen []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		w.Write([]byte(`{"action":"none","message":"ok"}`))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	var spec manifest.AppSpec
	for _, call := range []func() error{
		func() error { _, err := Apply(sock, spec, Options{Now: true}); return err },
		func() error { _, err := Apply(sock, spec, Options{}); return err },
		func() error { _, err := Restart(sock, spec, Options{Now: true}); return err },
		func() error { _, err := Restart(sock, spec, Options{}); return err },
		func() error { _, err := Stop(sock, spec); return err },
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"/apply?now=1", "/apply?", "/restart?now=1", "/restart?", "/stop?"}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(want) {
		t.Fatalf("requests = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, seen[i], want[i])
		}
	}
}

func TestShutdown_Endpoint(t *testing.T) {
	dir, err := os.MkdirTemp("", "api")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var seen []string
	code := http.StatusOK
	body := `{"pid":42,"apps":["a","b"],"balancer":true,"notes":["n"],"problems":["p"]}`
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.WriteHeader(code)
		w.Write([]byte(body))
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	resp, err := Shutdown(sock)
	if err != nil {
		t.Fatal(err)
	}
	if resp.PID != 42 || len(resp.Apps) != 2 || !resp.Balancer || len(resp.Notes) != 1 || len(resp.Problems) != 1 {
		t.Errorf("response = %+v", resp)
	}

	mu.Lock()
	code, body = http.StatusNotFound, "404 page not found\n"
	mu.Unlock()
	if _, err := Shutdown(sock); !errors.Is(err, ErrNoShutdown) {
		t.Errorf("404: err = %v, want ErrNoShutdown", err)
	}

	mu.Lock()
	code, body = http.StatusConflict, "it is already shutting down\n"
	mu.Unlock()
	if _, err := Shutdown(sock); err == nil || err.Error() != "shinyctl-api: it is already shutting down" {
		t.Errorf("409: err = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, s := range seen {
		if s != "POST /shutdown" {
			t.Errorf("request %q, want POST /shutdown", s)
		}
	}
	if len(seen) != 3 {
		t.Errorf("%d requests, want 3", len(seen))
	}
}
