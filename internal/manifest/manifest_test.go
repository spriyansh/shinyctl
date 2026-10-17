package manifest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeManifest(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "manifest.yaml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("writing test manifest: %v", err)
	}
	return path
}

func TestLoad_ResolvesRelativePaths(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `
app-group: g
app-name: n
entrypoint-file: ./app.R
port: 8881
replicas: 1
app-state-file: ./state.json
`)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	spec, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	wantEntrypoint := filepath.Join(wd, "app.R")
	if spec.EntrypointFile != wantEntrypoint {
		t.Errorf("EntrypointFile = %q, want %q", spec.EntrypointFile, wantEntrypoint)
	}

	wantStateFile := filepath.Join(wd, "state.json")
	if spec.AppStateFile != wantStateFile {
		t.Errorf("AppStateFile = %q, want %q", spec.AppStateFile, wantStateFile)
	}
}

func TestLoad_DefaultsWorkingDirToEntrypointDir(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `
app-group: g
app-name: n
entrypoint-file: ./nested/app.R
port: 8881
replicas: 1
app-state-file: ./state.json
`)

	spec, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	wantDir := filepath.Dir(spec.EntrypointFile)
	if spec.WorkingDir != wantDir {
		t.Errorf("WorkingDir = %q, want %q", spec.WorkingDir, wantDir)
	}
}

func TestLoad_ExplicitWorkingDirResolvedAbsolute(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `
app-group: g
app-name: n
entrypoint-file: ./app.R
port: 8881
replicas: 1
app-state-file: ./state.json
working-dir: ./somewhere/else
`)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	spec, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(wd, "somewhere/else")
	if spec.WorkingDir != want {
		t.Errorf("WorkingDir = %q, want %q", spec.WorkingDir, want)
	}
}

func TestValidate(t *testing.T) {
	// lookPath is faked for the whole test so none of these cases depend
	// on whether R is actually installed on whatever machine runs `go
	// test` — only the "Rscript missing" case below overrides it to fail.
	t.Cleanup(func() { lookPath = exec.LookPath })
	lookPath = func(string) (string, error) { return "/usr/bin/Rscript", nil }

	entrypoint := filepath.Join(t.TempDir(), "app.R")
	if err := os.WriteFile(entrypoint, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	base := AppSpec{
		AppGroup:       "g",
		AppName:        "n",
		EntrypointFile: entrypoint,
		Port:           8881,
		Replicas:       1,
	}

	cases := []struct {
		name    string
		mutate  func(s AppSpec) AppSpec
		wantErr bool
	}{
		{"valid as-is", func(s AppSpec) AppSpec { return s }, false},
		{"omitted replicas accepted", func(s AppSpec) AppSpec { s.Replicas = 0; return s }, false},
		{"replicas of 2 rejected", func(s AppSpec) AppSpec { s.Replicas = 2; return s }, true},
		{"negative replicas rejected", func(s AppSpec) AppSpec { s.Replicas = -1; return s }, true},
		{"omitted host accepted", func(s AppSpec) AppSpec { s.Host = ""; return s }, false},
		{"loopback host accepted", func(s AppSpec) AppSpec { s.Host = "127.0.0.1"; return s }, false},
		{"IPv6 host accepted", func(s AppSpec) AppSpec { s.Host = "::1"; return s }, false},
		{"hostname rejected as host", func(s AppSpec) AppSpec { s.Host = "localhost"; return s }, true},
		{"garbage host rejected", func(s AppSpec) AppSpec { s.Host = "0.0.0.0:8881"; return s }, true},
		{"restart-policy always is valid", func(s AppSpec) AppSpec { s.RestartPolicy = RestartAlways; return s }, false},
		{"restart-policy never is valid", func(s AppSpec) AppSpec { s.RestartPolicy = RestartNever; return s }, false},
		{"unknown restart-policy rejected", func(s AppSpec) AppSpec { s.RestartPolicy = "sometimes"; return s }, true},
		{"port 0 rejected", func(s AppSpec) AppSpec { s.Port = 0; return s }, true},
		{"port above 65535 rejected", func(s AppSpec) AppSpec { s.Port = 70000; return s }, true},
		{"missing entrypoint-file rejected", func(s AppSpec) AppSpec {
			s.EntrypointFile = filepath.Join(t.TempDir(), "does-not-exist.R")
			return s
		}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.mutate(base))
			if (err != nil) != c.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, c.wantErr)
			}
		})
	}

	t.Run("Rscript not on PATH rejected", func(t *testing.T) {
		t.Cleanup(func() { lookPath = exec.LookPath })
		lookPath = func(string) (string, error) { return "", fmt.Errorf("not found") }

		if err := Validate(base); err == nil {
			t.Error("Validate() = nil, want an error when Rscript isn't on PATH")
		}
	})
}

func TestLoad_Env(t *testing.T) {
	dir := t.TempDir()
	path := writeManifest(t, dir, `
app-group: g
app-name: n
entrypoint-file: ./app.R
port: 8881
replicas: 1
app-state-file: ./state.json
env:
  R_LIBS_USER: /home/me/Rlibs
  API_KEY: secret
`)

	spec, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if spec.Env["R_LIBS_USER"] != "/home/me/Rlibs" || spec.Env["API_KEY"] != "secret" {
		t.Errorf("Env = %v, missing expected entries", spec.Env)
	}
}
