package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// PageSlow is how long a copy's page may take before the wait for it is
// reported, in status and api.log. It is not a timeout: the request is kept
// until the page answers or the process is gone, because R renders every
// request it was sent, also one whose client gave up, so giving up would only
// queue another render behind it, and a page slower than any timeout would
// never be ready.
const PageSlow = 10 * time.Second

// pageRead is how much of the page is read before the connection is closed.
const pageRead = 64 << 10

// Page is what one request for a copy's page found.
type Page struct {
	Listening bool   // something accepted the connection
	Ready     bool   // the page answered with a status below 500
	Problem   string // for a copy that listens and is not ready: what its page does instead, read after "its page"
}

// pageClient: no proxy whatever the environment says (the copies are on this
// machine), no connection kept to a copy between requests, no redirect
// followed (a redirect is an answer, not something to fetch), the family
// chosen as Listening does.
var pageClient = &http.Client{
	Transport: &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			return (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, tcpNetwork(host), addr)
		},
	},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// CheckPage asks the copy at host:port for its page, http://host:port/, and
// says what it found. Any answer below 500 makes a copy ready: Shiny answers
// 500 with its error page when the app's ui fails, while a redirect to a
// sign-in page, or a 401 or 403 to a request without credentials, is the app
// working. It is a GET, what a visitor sends; a little of the body is read
// and the connection closed, so no Shiny session is opened (a session is a
// WebSocket) and nothing stays open to the copy. It has no timeout of its
// own: it returns when the copy answers, the connection fails, or ctx ends.
func CheckPage(ctx context.Context, host string, port int) Page {
	if !Listening(host, port) {
		return Page{}
	}
	target := "http://" + net.JoinHostPort(dialHost(host), strconv.Itoa(port)) + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Page{Listening: true, Problem: err.Error()}
	}
	req.Header.Set("User-Agent", "shinyctl")
	req.Header.Set("Accept", "text/html")
	resp, err := pageClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Page{Listening: true, Problem: "has not answered"}
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is ours, not news
		}
		return Page{Listening: true, Problem: fmt.Sprintf("does not answer with HTTP (%v)", err)}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, pageRead))
	if resp.StatusCode >= 500 {
		return Page{Listening: true, Problem: "answers HTTP " + strconv.Itoa(resp.StatusCode)}
	}
	return Page{Listening: true, Ready: true}
}
