package process

import "testing"

func TestBuildEnv(t *testing.T) {
	t.Setenv("SHINYCTL_TEST_VAR", "inherited")

	got := buildEnv(map[string]string{
		"SHINYCTL_TEST_VAR": "overridden",
		"SHINYCTL_NEW_VAR":  "added",
	})

	values := map[string]int{}
	for _, kv := range got {
		values[kv]++
	}

	if values["SHINYCTL_TEST_VAR=overridden"] != 1 {
		t.Errorf("expected exactly one SHINYCTL_TEST_VAR=overridden entry, got %v", got)
	}
	if n := countPrefix(got, "SHINYCTL_TEST_VAR="); n != 1 {
		t.Errorf("SHINYCTL_TEST_VAR appears %d times, want exactly 1 (no duplicate key)", n)
	}
	if values["SHINYCTL_NEW_VAR=added"] != 1 {
		t.Errorf("expected SHINYCTL_NEW_VAR=added to be present, got %v", got)
	}
}

func countPrefix(env []string, prefix string) int {
	n := 0
	for _, kv := range env {
		if len(kv) >= len(prefix) && kv[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}
