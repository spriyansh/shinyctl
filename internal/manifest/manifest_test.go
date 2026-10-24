package manifest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestLoad_ResolvesRelativePathsFromManifestDir(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()

	cases := []struct {
		name           string
		body           string
		wantEntrypoint string
		wantStateFile  string
		wantWorkingDir string
	}{
		{
			name: "dot-slash paths",
			body: `
entrypoint-file: ./app.R
app-state-file: ./state.json
`,
			wantEntrypoint: filepath.Join(dir, "app.R"),
			wantStateFile:  filepath.Join(dir, "state.json"),
			wantWorkingDir: dir,
		},
		{
			name: "explicit working-dir",
			body: `
entrypoint-file: ./app.R
app-state-file: ./state.json
working-dir: ./somewhere/else
`,
			wantEntrypoint: filepath.Join(dir, "app.R"),
			wantStateFile:  filepath.Join(dir, "state.json"),
			wantWorkingDir: filepath.Join(dir, "somewhere/else"),
		},
		{
			name: "parent directory",
			body: `
entrypoint-file: ../shared/app.R
app-state-file: ./state.json
`,
			wantEntrypoint: filepath.Join(filepath.Dir(dir), "shared/app.R"),
			wantStateFile:  filepath.Join(dir, "state.json"),
			wantWorkingDir: filepath.Join(filepath.Dir(dir), "shared"),
		},
		{
			name: "tilde is not expanded",
			body: `
entrypoint-file: ~/app.R
app-state-file: ./state.json
`,
			wantEntrypoint: filepath.Join(dir, "~/app.R"),
			wantStateFile:  filepath.Join(dir, "state.json"),
			wantWorkingDir: filepath.Join(dir, "~"),
		},
		{
			name:           "absolute paths are only cleaned",
			body:           "\nentrypoint-file: " + other + "/x/../app.R\napp-state-file: " + other + "/state.json\nworking-dir: " + other + "/w/../wd\n",
			wantEntrypoint: filepath.Join(other, "app.R"),
			wantStateFile:  filepath.Join(other, "state.json"),
			wantWorkingDir: filepath.Join(other, "wd"),
		},
		{
			name:           "missing fields stay empty",
			body:           "",
			wantEntrypoint: "",
			wantStateFile:  "",
			wantWorkingDir: ".",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeManifest(t, dir, "app-group: g\napp-name: n\nport: 8881\n"+c.body)

			spec, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}

			if spec.EntrypointFile != c.wantEntrypoint {
				t.Errorf("EntrypointFile = %q, want %q", spec.EntrypointFile, c.wantEntrypoint)
			}
			if spec.AppStateFile != c.wantStateFile {
				t.Errorf("AppStateFile = %q, want %q", spec.AppStateFile, c.wantStateFile)
			}
			if spec.WorkingDir != c.wantWorkingDir {
				t.Errorf("WorkingDir = %q, want %q", spec.WorkingDir, c.wantWorkingDir)
			}
		})
	}
}

func TestLoad_RelativeManifestPath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, sub, `
app-group: g
app-name: n
entrypoint-file: ./app.R
port: 8881
app-state-file: ./state.json
`)

	t.Chdir(root)

	spec, err := Load(filepath.Join("sub", "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(root, "sub", "app.R"); spec.EntrypointFile != want {
		t.Errorf("EntrypointFile = %q, want %q", spec.EntrypointFile, want)
	}
	if want := filepath.Join(root, "sub", "state.json"); spec.AppStateFile != want {
		t.Errorf("AppStateFile = %q, want %q", spec.AppStateFile, want)
	}
}

// A manifest reached through a symlink resolves from where the link is, not
// from the file it points at: otherwise every link to one shared template
// would collapse into the template's folder and share one app-state-file,
// which is one app identity.
func TestLoad_SymlinkedManifestResolvesFromLink(t *testing.T) {
	body := `
app-group: g
app-name: n
entrypoint-file: ./app.R
port: 8881
app-state-file: ./state.json
`

	t.Run("file link", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real")
		if err := os.Mkdir(real, 0755); err != nil {
			t.Fatal(err)
		}
		target := writeManifest(t, real, body)
		link := filepath.Join(dir, "app.yaml")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}

		spec, err := Load(link)
		if err != nil {
			t.Fatal(err)
		}

		if want := filepath.Join(dir, "app.R"); spec.EntrypointFile != want {
			t.Errorf("EntrypointFile = %q, want %q", spec.EntrypointFile, want)
		}
	})

	t.Run("directory link", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "real")
		if err := os.Mkdir(real, 0755); err != nil {
			t.Fatal(err)
		}
		writeManifest(t, real, body)
		if err := os.Symlink(real, filepath.Join(dir, "linked")); err != nil {
			t.Fatal(err)
		}

		spec, err := Load(filepath.Join(dir, "linked", "manifest.yaml"))
		if err != nil {
			t.Fatal(err)
		}

		if want := filepath.Join(dir, "linked", "app.R"); spec.EntrypointFile != want {
			t.Errorf("EntrypointFile = %q, want %q", spec.EntrypointFile, want)
		}
	})
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

	wantDir := filepath.Join(dir, "nested")
	if spec.WorkingDir != wantDir {
		t.Errorf("WorkingDir = %q, want %q", spec.WorkingDir, wantDir)
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
		{"port just below the internal range accepted", func(s AppSpec) AppSpec { s.Port = 19999; return s }, false},
		{"first internal port rejected", func(s AppSpec) AppSpec { s.Port = 20000; return s }, true},
		{"port inside the internal range rejected", func(s AppSpec) AppSpec { s.Port = 25000; return s }, true},
		{"last internal port rejected", func(s AppSpec) AppSpec { s.Port = 29999; return s }, true},
		{"port just above the internal range accepted", func(s AppSpec) AppSpec { s.Port = 30000; return s }, false},
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

	t.Run("the messages", func(t *testing.T) {
		spec := base
		spec.Port = 25000
		want := "port must not be between 20000 and 29999, which shinyctl keeps for the apps' own processes"
		if err := Validate(spec); err == nil || err.Error() != want {
			t.Errorf("reserved port: %v, want %q", err, want)
		}

		spec = base
		spec.Replicas = 2
		want = "replicas must be 1 or omitted — shinyctl runs one copy of each app for now"
		if err := Validate(spec); err == nil || err.Error() != want {
			t.Errorf("replicas: %v, want %q", err, want)
		}
	})

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

func TestStartFields(t *testing.T) {
	base := AppSpec{
		AppGroup:   "g",
		AppName:    "n",
		Host:       "127.0.0.1",
		WorkingDir: "/srv/app",
		Env:        map[string]string{"API_KEY": "s3cret", "R_LIBS_USER": "/home/me/Rlibs"},
	}

	t.Run("exactly the fields the record does not already hold", func(t *testing.T) {
		got := base.StartFields()
		for _, key := range []string{"app-group", "working-dir", "env"} {
			if _, ok := got[key]; !ok {
				t.Errorf("StartFields() has no %q", key)
			}
		}
		if _, ok := got["host"]; ok {
			t.Error("StartFields() has host, which only moves the load balancer's listener")
		}
		if len(got) != 3 {
			t.Errorf("StartFields() = %v, want exactly 3 keys", got)
		}
	})

	t.Run("a nil env is an empty env", func(t *testing.T) {
		a, b := base, base
		a.Env = nil
		b.Env = map[string]string{}
		if a.StartFields()["env"] != b.StartFields()["env"] {
			t.Error("nil and empty env differ")
		}
	})

	t.Run("env is compared by content", func(t *testing.T) {
		reordered := base
		reordered.Env = map[string]string{"R_LIBS_USER": "/home/me/Rlibs", "API_KEY": "s3cret"}
		if reordered.StartFields()["env"] != base.StartFields()["env"] {
			t.Error("the same variables in another order differ")
		}

		changed := base
		changed.Env = map[string]string{"API_KEY": "other", "R_LIBS_USER": "/home/me/Rlibs"}
		if changed.StartFields()["env"] == base.StartFields()["env"] {
			t.Error("a changed value left the digest unchanged")
		}
	})

	t.Run("env values never appear in the record", func(t *testing.T) {
		for key, value := range base.StartFields() {
			if strings.Contains(value, "s3cret") || strings.Contains(value, "Rlibs") {
				t.Errorf("field %q = %q contains an env value", key, value)
			}
		}
	})
}
