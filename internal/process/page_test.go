package process

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hostPort splits a test server's address.
func hostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := net.LookupPort("tcp", p)
	if err != nil {
		t.Fatal(err)
	}
	return host, port
}

// silentListener accepts connections and never writes to them.
func silentListener(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	return hostPort(t, ln.Addr().String())
}

func TestCheckPage(t *testing.T) {
	ctx := context.Background()

	serve := func(t *testing.T, h http.HandlerFunc) (string, int) {
		t.Helper()
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return hostPort(t, srv.Listener.Addr().String())
	}

	t.Run("200 is ready", func(t *testing.T) {
		host, port := serve(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/" || !r.Close {
				t.Errorf("request = %s %s close=%v, want GET / with the connection closed", r.Method, r.URL.Path, r.Close)
			}
			w.Write([]byte("<html></html>"))
		})
		got := CheckPage(ctx, host, port)
		if !got.Listening || !got.Ready || got.Problem != "" {
			t.Errorf("CheckPage = %+v, want ready", got)
		}
	})

	for _, code := range []int{500, 503} {
		t.Run(strconv.Itoa(code)+" is not ready", func(t *testing.T) {
			host, port := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) })
			got := CheckPage(ctx, host, port)
			want := "answers HTTP " + strconv.Itoa(code)
			if !got.Listening || got.Ready || got.Problem != want {
				t.Errorf("CheckPage = %+v, want listening, not ready, %q", got, want)
			}
		})
	}

	t.Run("a redirect is an answer and is not followed", func(t *testing.T) {
		host, port := serve(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				t.Errorf("the redirect to %s was followed", r.URL.Path)
			}
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		})
		if got := CheckPage(ctx, host, port); !got.Ready {
			t.Errorf("CheckPage = %+v, want ready", got)
		}
	})

	for _, code := range []int{401, 404} {
		t.Run(strconv.Itoa(code)+" is ready", func(t *testing.T) {
			host, port := serve(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) })
			if got := CheckPage(ctx, host, port); !got.Ready {
				t.Errorf("CheckPage = %+v, want ready", got)
			}
		})
	}

	t.Run("a slow page is waited for as long as ctx lasts", func(t *testing.T) {
		host, port := serve(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) })

		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		got := CheckPage(short, host, port)
		if !got.Listening || got.Ready || got.Problem != "has not answered" {
			t.Errorf("with 100ms: CheckPage = %+v, want listening, not ready, has not answered", got)
		}

		long, cancel2 := context.WithTimeout(ctx, 2*time.Second)
		defer cancel2()
		if got := CheckPage(long, host, port); !got.Ready {
			t.Errorf("with 2s: CheckPage = %+v, want ready", got)
		}
	})

	t.Run("a closed port is not listening", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		host, port := hostPort(t, ln.Addr().String())
		ln.Close()
		if got := CheckPage(ctx, host, port); got != (Page{}) {
			t.Errorf("CheckPage = %+v, want the zero Page", got)
		}
	})

	t.Run("a listener that never writes has not answered", func(t *testing.T) {
		host, port := silentListener(t)
		short, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		got := CheckPage(short, host, port)
		if !got.Listening || got.Ready || got.Problem != "has not answered" {
			t.Errorf("CheckPage = %+v, want listening, not ready, has not answered", got)
		}
	})

	t.Run("a cancelled ctx ends the wait", func(t *testing.T) {
		host, port := silentListener(t)
		c, cancel := context.WithCancel(ctx)
		time.AfterFunc(50*time.Millisecond, cancel)
		done := make(chan Page, 1)
		go func() { done <- CheckPage(c, host, port) }()
		select {
		case got := <-done:
			if got.Ready {
				t.Errorf("CheckPage = %+v, want not ready", got)
			}
		case <-time.After(time.Second):
			t.Fatal("CheckPage did not return within 1s of the cancel")
		}
	})

	t.Run("something that does not speak HTTP", func(t *testing.T) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Write([]byte("nope\r\n\r\n"))
				c.Close()
			}
		}()
		host, port := hostPort(t, ln.Addr().String())
		got := CheckPage(ctx, host, port)
		if !got.Listening || got.Ready {
			t.Fatalf("CheckPage = %+v, want listening and not ready", got)
		}
		if !strings.HasPrefix(got.Problem, "does not answer with HTTP (") || strings.Contains(got.Problem, "http://") {
			t.Errorf("Problem = %q, want it to start with %q and leave the URL out", got.Problem, "does not answer with HTTP (")
		}
	})

	t.Run("a large page is cut short", func(t *testing.T) {
		host, port := serve(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(strings.Repeat("x", 4<<20)))
		})
		start := time.Now()
		got := CheckPage(ctx, host, port)
		if !got.Ready {
			t.Errorf("CheckPage = %+v, want ready", got)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("CheckPage took %s on a 4 MiB page", d)
		}
	})
}
