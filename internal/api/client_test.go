package api

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/reconcile"
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

// A shinyctl-api that does not answer is given up on, whether it says nothing
// at all or sends the headers and then stalls on the body.
func TestStatus_GivesUpOnASilentDaemon(t *testing.T) {
	old := statusTimeout
	statusTimeout = 100 * time.Millisecond
	t.Cleanup(func() { statusTimeout = old })
	want := "shinyctl-api did not answer within " + reconcile.FormatDuration(statusTimeout)

	for _, tc := range []struct {
		name  string
		serve func(ln net.Listener, done <-chan struct{})
	}{
		{"no answer at all", func(ln net.Listener, done <-chan struct{}) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() { <-done; c.Close() }()
			}
		}},
		{"headers then a stalled body", func(ln net.Listener, done <-chan struct{}) {
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "1000")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-done:
				}
			})}
			go func() { <-done; srv.Close() }()
			srv.Serve(ln)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "api")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			ln, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go tc.serve(ln, done)
			t.Cleanup(func() { close(done); ln.Close() })

			_, err = Status(filepath.Join(dir, "s.sock"))
			if err == nil || err.Error() != want {
				t.Fatalf("Status error = %v, want %q", err, want)
			}
		})
	}
}
