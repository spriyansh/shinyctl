package lb

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
)

// These tests share the package's table with every other test here, so none
// of them runs in parallel.

func resetTable(t *testing.T) {
	t.Helper()
	Set(nil)
	t.Cleanup(func() { Set(nil) })
}

func TestSnapshot_OrderAndRoles(t *testing.T) {
	resetTable(t)
	Set(Table{
		"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"c:3", "a:1"}, Draining: []string{"d:4", "c:3"}},
	})
	s := load()

	if got, want := s.order["x"], []string{"a:1", "b:2", "c:3", "d:4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	for dial, want := range map[string]role{"a:1": roleActive, "b:2": roleActive, "c:3": roleWarm, "d:4": roleDraining, "e:5": roleNone} {
		if got := s.roles["x"][dial]; got != want {
			t.Errorf("role of %s = %d, want %d (a dial listed twice keeps its first role)", dial, got, want)
		}
	}
	if got := s.order["unknown"]; len(got) != 0 {
		t.Errorf("order of an unknown app = %v, want none", got)
	}
}

func TestTable_Equal(t *testing.T) {
	a := Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"c:3"}}}
	cases := []struct {
		name string
		u    Table
		want bool
	}{
		{"same", Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"c:3"}}}, true},
		{"different order", Table{"x": {Active: []string{"b:2", "a:1"}, Warm: []string{"c:3"}}}, false},
		{"different role", Table{"x": {Active: []string{"a:1"}, Warm: []string{"b:2", "c:3"}}}, false},
		{"only the warm list differs", Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"d:4"}}}, false},
		{"only the draining list differs", Table{"x": {Active: []string{"a:1", "b:2"}, Warm: []string{"c:3"}, Draining: []string{"d:4"}}}, false},
		{"extra key", Table{"x": a["x"], "y": {}}, false},
		{"missing key", Table{}, false},
		{"other key", Table{"y": a["x"]}, false},
	}
	for _, tc := range cases {
		if got := a.Equal(tc.u); got != tc.want {
			t.Errorf("%s: Equal = %v, want %v", tc.name, got, tc.want)
		}
		if got := tc.u.Equal(a); got != tc.want {
			t.Errorf("%s (reversed): Equal = %v, want %v", tc.name, got, tc.want)
		}
	}

	if !(Table{"x": {Active: nil}}).Equal(Table{"x": {Active: []string{}, Warm: []string{}}}) {
		t.Error("a nil list and an empty one should be equal")
	}
	if !(Table(nil)).Equal(Table{}) {
		t.Error("a nil table and an empty one should be equal")
	}
}

func TestTable_CloneSharesNothing(t *testing.T) {
	a := Table{"x": {Active: []string{"a:1"}}}
	b := a.Clone()
	b["x"].Active[0] = "changed:9"
	if a["x"].Active[0] != "a:1" {
		t.Error("changing a clone changed the original")
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(nil); err != nil {
		t.Errorf("an empty table: %v", err)
	}
	if err := Validate(Table{"x": {Active: []string{"127.0.0.1:20000"}, Warm: []string{"[::1]:20001"}}}); err != nil {
		t.Errorf("a good table: %v", err)
	}

	err := Validate(Table{"x": {Active: []string{"nonsense"}}})
	if err == nil || !strings.Contains(err.Error(), `"nonsense"`) {
		t.Errorf("nonsense: err = %v, want one naming the dial", err)
	}

	err = Validate(Table{"x": {Active: []string{"a:1"}, Warm: []string{"a:1"}}})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a dial in two roles: err = %v, want a duplicate", err)
	}

	// The same address in two apps is not a duplicate.
	if err := Validate(Table{"x": {Active: []string{"a:1"}}, "y": {Active: []string{"a:1"}}}); err != nil {
		t.Errorf("one dial in two apps: %v", err)
	}
}

func TestLoadFile(t *testing.T) {
	resetTable(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "copies.json")

	Set(Table{"stale": {Active: []string{"a:1"}}})
	if err := LoadFile(path); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	if got := Current(); len(got) != 0 {
		t.Errorf("missing file: table = %v, want empty", got)
	}

	Set(Table{"stale": {Active: []string{"a:1"}}})
	os.WriteFile(path, []byte("{not json"), 0o600)
	if err := LoadFile(path); err == nil {
		t.Error("invalid JSON: want an error")
	}
	if got := Current(); len(got) != 0 {
		t.Errorf("invalid JSON: table = %v, want empty", got)
	}

	Set(Table{"stale": {Active: []string{"a:1"}}})
	os.WriteFile(path, []byte(`{"x":{"active":["nonsense"]}}`), 0o600)
	if err := LoadFile(path); err == nil {
		t.Error("an invalid dial: want an error")
	}
	if got := Current(); len(got) != 0 {
		t.Errorf("an invalid dial: table = %v, want empty", got)
	}

	want := Table{"x": {Active: []string{"127.0.0.1:20000"}, Warm: []string{"127.0.0.1:20001"}}}
	os.WriteFile(path, []byte(`{"x":{"active":["127.0.0.1:20000"],"warm":["127.0.0.1:20001"]}}`), 0o600)
	if err := LoadFile(path); err != nil {
		t.Fatalf("valid file: %v", err)
	}
	if got := Current(); !got.Equal(want) {
		t.Errorf("valid file: table = %v, want %v", got, want)
	}
}

func TestCurrent_IsACopy(t *testing.T) {
	resetTable(t)
	Set(Table{"x": {Active: []string{"a:1"}}})
	got := Current()
	got["x"].Active[0] = "changed:9"
	delete(got, "x")
	if !Current().Equal(Table{"x": {Active: []string{"a:1"}}}) {
		t.Error("changing the result of Current changed the table")
	}
}

func TestCopies_FreshUpstreams(t *testing.T) {
	resetTable(t)
	Set(Table{"x": {Active: []string{"a:1"}, Warm: []string{"b:2"}, Draining: []string{"c:3"}}})
	src := &Copies{App: "x"}

	first, err := src.GetUpstreams(nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := src.GetUpstreams(nil)

	dials := func(us []*reverseproxy.Upstream) (out []string) {
		for _, u := range us {
			out = append(out, u.Dial)
		}
		return out
	}
	if got, want := dials(first), []string{"a:1", "b:2", "c:3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dials = %v, want %v (active, warm, draining)", got, want)
	}
	for i := range first {
		if first[i] == second[i] {
			t.Errorf("upstream %d is the same pointer in two calls; the handler would share state between requests", i)
		}
	}

	none, err := (&Copies{App: "unknown"}).GetUpstreams(nil)
	if err != nil || len(none) != 0 || none == nil {
		t.Errorf("an unknown app: %v, %v; want an empty list and no error", none, err)
	}
}
