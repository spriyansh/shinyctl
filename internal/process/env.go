package process

import (
	"os"
	"strings"
)

// buildEnv returns the environment a launched app should run with:
// shinyctl-api's own environment, with extra overlaid on top. Built via a
// map rather than simply appending "KEY=VALUE" pairs after os.Environ(),
// so a key present in both never ends up duplicated in the resulting
// slice — exec.Cmd.Env's behavior on a duplicate key isn't something
// worth depending on.
func buildEnv(extra map[string]string) []string {
	env := make(map[string]string, len(extra))
	for _, kv := range os.Environ() {
		key, value, found := strings.Cut(kv, "=")
		if found {
			env[key] = value
		}
	}

	for k, v := range extra {
		env[k] = v
	}

	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, k+"="+v)
	}

	return result
}
