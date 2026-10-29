package lb

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2"
)

func callAdmin(method, body string) (*httptest.ResponseRecorder, error) {
	r := httptest.NewRequest(method, AdminPath, strings.NewReader(body))
	w := httptest.NewRecorder()
	err := handleCopies(w, r)
	return w, err
}

func statusOf(err error) int {
	var apiErr caddy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatus
	}
	return 0
}

func TestAdmin_GetReturnsTheTable(t *testing.T) {
	resetTable(t)
	Set(Table{"x": {Active: []string{"a:1"}, Warm: []string{"b:2"}}})

	w, err := callAdmin(http.MethodGet, "")
	if err != nil {
		t.Fatal(err)
	}
	var got Table
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(Table{"x": {Active: []string{"a:1"}, Warm: []string{"b:2"}}}) {
		t.Errorf("GET = %s", w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestAdmin_PutReplacesTheTable(t *testing.T) {
	resetTable(t)
	Set(Table{"old": {Active: []string{"a:1"}}})

	if _, err := callAdmin(http.MethodPut, `{"x":{"active":["127.0.0.1:20000"],"warm":["127.0.0.1:20001"]}}`); err != nil {
		t.Fatal(err)
	}
	if !Current().Equal(Table{"x": {Active: []string{"127.0.0.1:20000"}, Warm: []string{"127.0.0.1:20001"}}}) {
		t.Errorf("table after PUT = %v", Current())
	}
}

func TestAdmin_BadPutChangesNothing(t *testing.T) {
	resetTable(t)
	before := Table{"x": {Active: []string{"a:1"}}}
	Set(before)

	for name, body := range map[string]string{
		"invalid JSON": `{"x":`,
		"invalid dial": `{"y":{"active":["nonsense"]}}`,
		"a duplicate":  `{"y":{"active":["a:1"],"warm":["a:1"]}}`,
	} {
		_, err := callAdmin(http.MethodPut, body)
		if got := statusOf(err); got != http.StatusBadRequest {
			t.Errorf("%s: status = %d (err %v), want 400", name, got, err)
		}
		if !Current().Equal(before) {
			t.Errorf("%s: the table changed to %v", name, Current())
		}
	}
}

func TestAdmin_OtherMethodsAreRefused(t *testing.T) {
	resetTable(t)
	for _, m := range []string{http.MethodDelete, http.MethodPost, http.MethodPatch} {
		_, err := callAdmin(m, "")
		if got := statusOf(err); got != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", m, got)
		}
	}
}

func TestAdmin_RoutesAreMounted(t *testing.T) {
	routes := adminCopies{}.Routes()
	if len(routes) != 1 || routes[0].Pattern != AdminPath {
		t.Errorf("routes = %+v, want one at %s", routes, AdminPath)
	}
}

func TestAdmin_NamesTheServingProcess(t *testing.T) {
	resetTable(t)
	t.Cleanup(func() { MarkServing(0) })

	w, err := callAdmin(http.MethodGet, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Header().Get(PIDHeader); got != "" {
		t.Errorf("%s = %q before the process serves, want none", PIDHeader, got)
	}

	MarkServing(4242)
	w, err = callAdmin(http.MethodGet, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := w.Header().Get(PIDHeader); got != "4242" {
		t.Errorf("%s = %q after MarkServing(4242), want 4242", PIDHeader, got)
	}
}
