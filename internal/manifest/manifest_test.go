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

	zero, sixteen, seventeen, minusOne := 0, 16, 17, -1

	cases := []struct {
		name    string
		mutate  func(s AppSpec) AppSpec
		wantErr bool
	}{
		{"valid as-is", func(s AppSpec) AppSpec { return s }, false},
		{"omitted replicas accepted", func(s AppSpec) AppSpec { s.Replicas = 0; return s }, false},
		{"replicas of 16 accepted", func(s AppSpec) AppSpec { s.Replicas = 16; return s }, false},
		{"replicas of 17 rejected", func(s AppSpec) AppSpec { s.Replicas = 17; return s }, true},
		{"negative replicas rejected", func(s AppSpec) AppSpec { s.Replicas = -1; return s }, true},
		{"warm-replicas of 0 accepted", func(s AppSpec) AppSpec { s.WarmReplicas = &zero; return s }, false},
		{"warm-replicas of 16 accepted", func(s AppSpec) AppSpec { s.WarmReplicas = &sixteen; return s }, false},
		{"warm-replicas of 17 rejected", func(s AppSpec) AppSpec { s.WarmReplicas = &seventeen; return s }, true},
		{"negative warm-replicas rejected", func(s AppSpec) AppSpec { s.WarmReplicas = &minusOne; return s }, true},
		{"lb-policy least-conn accepted", func(s AppSpec) AppSpec { s.LBPolicy = "least-conn"; return s }, false},
		{"lb-policy round-robin accepted", func(s AppSpec) AppSpec { s.LBPolicy = "round-robin"; return s }, false},
		{"lb-policy random accepted", func(s AppSpec) AppSpec { s.LBPolicy = "random"; return s }, false},
		{"lb-policy first accepted", func(s AppSpec) AppSpec { s.LBPolicy = "first"; return s }, false},
		{"the engine's spelling least_conn rejected", func(s AppSpec) AppSpec { s.LBPolicy = "least_conn"; return s }, true},
		{"unknown lb-policy rejected", func(s AppSpec) AppSpec { s.LBPolicy = "ip-hash"; return s }, true},
		{"max-memory of 512M accepted", func(s AppSpec) AppSpec { s.MaxMemory = "512M"; return s }, false},
		{"max-memory of 128M accepted", func(s AppSpec) AppSpec { s.MaxMemory = "128M"; return s }, false},
		{"max-memory of 1.5Gi accepted", func(s AppSpec) AppSpec { s.MaxMemory = "1.5Gi"; return s }, false},
		{"max-memory of 127M rejected", func(s AppSpec) AppSpec { s.MaxMemory = "127M"; return s }, true},
		{"max-memory without a unit rejected", func(s AppSpec) AppSpec { s.MaxMemory = "512"; return s }, true},
		{"max-memory of 0 rejected", func(s AppSpec) AppSpec { s.MaxMemory = "0"; return s }, true},
		{"max-memory of 0M rejected", func(s AppSpec) AppSpec { s.MaxMemory = "0M"; return s }, true},
		{"max-memory that is not a size rejected", func(s AppSpec) AppSpec { s.MaxMemory = "lots"; return s }, true},
		{"max-memory that overflows rejected", func(s AppSpec) AppSpec { s.MaxMemory = "99999999999T"; return s }, true},
		{"drain-timeout 0 accepted", func(s AppSpec) AppSpec { s.DrainTimeout = "0"; return s }, false},
		{"drain-timeout 0s accepted", func(s AppSpec) AppSpec { s.DrainTimeout = "0s"; return s }, false},
		{"drain-timeout 90s accepted", func(s AppSpec) AppSpec { s.DrainTimeout = "90s"; return s }, false},
		{"drain-timeout 15m accepted", func(s AppSpec) AppSpec { s.DrainTimeout = "15m"; return s }, false},
		{"drain-timeout 1h30m accepted", func(s AppSpec) AppSpec { s.DrainTimeout = "1h30m"; return s }, false},
		{"drain-timeout 24h accepted", func(s AppSpec) AppSpec { s.DrainTimeout = "24h"; return s }, false},
		{"drain-timeout 25h rejected", func(s AppSpec) AppSpec { s.DrainTimeout = "25h"; return s }, true},
		{"drain-timeout -1m rejected", func(s AppSpec) AppSpec { s.DrainTimeout = "-1m"; return s }, true},
		{"drain-timeout 900 rejected", func(s AppSpec) AppSpec { s.DrainTimeout = "900"; return s }, true},
		{"drain-timeout abc rejected", func(s AppSpec) AppSpec { s.DrainTimeout = "abc"; return s }, true},
		{"a space in the entrypoint path accepted", func(s AppSpec) AppSpec { s.WorkingDir = "/srv/my apps"; return s }, false},
		{"a tab in the working dir is Validate's business no more", func(s AppSpec) AppSpec { s.WorkingDir = "/srv/a\tb"; return s }, false},
		{"app-name with @ rejected", func(s AppSpec) AppSpec { s.AppName = "a@2"; return s }, true},
		{"app-name with / rejected", func(s AppSpec) AppSpec { s.AppName = "a/b"; return s }, true},
		{"app-group with @ rejected", func(s AppSpec) AppSpec { s.AppGroup = "g@x"; return s }, true},
		{"app-group with / rejected", func(s AppSpec) AppSpec { s.AppGroup = "g/x"; return s }, true},
		{"app-name with a dash and a dot accepted", func(s AppSpec) AppSpec { s.AppName = "my-app.v2"; return s }, false},
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

		for name, c := range map[string]struct {
			mutate func(*AppSpec)
			want   string
		}{
			"replicas":      {func(s *AppSpec) { s.Replicas = 17 }, "replicas must be between 1 and 16"},
			"warm-replicas": {func(s *AppSpec) { s.WarmReplicas = &seventeen }, "warm-replicas must be between 0 and 16"},
			"lb-policy":     {func(s *AppSpec) { s.LBPolicy = "least_conn" }, `lb-policy must be one of "least-conn", "round-robin", "random", "first"`},
			"app-name":      {func(s *AppSpec) { s.AppName = "a@b" }, `app-name must not contain "/" or "@"`},
			"app-group":     {func(s *AppSpec) { s.AppGroup = "g/h" }, `app-group must not contain "/" or "@"`},
		} {
			spec = base
			c.mutate(&spec)
			if err := Validate(spec); err == nil || err.Error() != c.want {
				t.Errorf("%s: %v, want %q", name, err, c.want)
			}
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

func TestCheckPathCharacters(t *testing.T) {
	good := AppSpec{EntrypointFile: "/srv/my apps/app.R", WorkingDir: "/srv/my apps"}
	if err := CheckPathCharacters(good); err != nil {
		t.Errorf("a path with a space: %v", err)
	}
	for name, c := range map[string]struct {
		mutate func(*AppSpec)
		want   string
	}{
		"newline in the entrypoint": {func(s *AppSpec) { s.EntrypointFile += "\n" }, "entrypoint-file must not contain control characters such as tabs or line breaks"},
		"tab in the entrypoint":     {func(s *AppSpec) { s.EntrypointFile = "/a\tb.R" }, "entrypoint-file must not contain control characters such as tabs or line breaks"},
		"tab in the working dir":    {func(s *AppSpec) { s.WorkingDir = "/srv/a\tb" }, "working-dir must not contain control characters such as tabs or line breaks"},
		"escape in the working dir": {func(s *AppSpec) { s.WorkingDir = "/srv/\x1b" }, "working-dir must not contain control characters such as tabs or line breaks"},
	} {
		spec := good
		c.mutate(&spec)
		if err := CheckPathCharacters(spec); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestValidate_DrainTimeoutMessage(t *testing.T) {
	t.Cleanup(func() { lookPath = exec.LookPath })
	lookPath = func(string) (string, error) { return "/usr/bin/Rscript", nil }
	entrypoint := filepath.Join(t.TempDir(), "app.R")
	if err := os.WriteFile(entrypoint, nil, 0644); err != nil {
		t.Fatal(err)
	}
	spec := AppSpec{AppGroup: "g", AppName: "n", EntrypointFile: entrypoint, Port: 8881, DrainTimeout: "900"}
	err := Validate(spec)
	want := `drain-timeout must be a duration from 0 to 24h, such as 15m, 90s or 1h (got "900")`
	if err == nil || err.Error() != want {
		t.Errorf("Validate = %v, want %q", err, want)
	}
}

// TestValidate_MaxMemory: each refusal says what to do about it.
func TestValidate_MaxMemory(t *testing.T) {
	t.Cleanup(func() { lookPath = exec.LookPath })
	lookPath = func(string) (string, error) { return "/usr/bin/Rscript", nil }
	entrypoint := filepath.Join(t.TempDir(), "app.R")
	if err := os.WriteFile(entrypoint, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ in, want string }{
		{"512", `max-memory must be a size with a unit, such as 512M or 2G (got "512")`},
		{"lots", `max-memory must be a size with a unit, such as 512M or 2G (got "lots")`},
		{"0", "remove max-memory for no limit"},
		{"0.0000000001M", "remove max-memory for no limit"},
		{"127M", "max-memory must be at least 128M: an idle Shiny copy already holds about 70-100M"},
		{"99999999999T", `max-memory is too large (got "99999999999T")`},
		{"0.0001M", "max-memory must be at least 128M"},
	} {
		spec := AppSpec{AppGroup: "g", AppName: "n", EntrypointFile: entrypoint, Port: 8881, Replicas: 1, MaxMemory: c.in}
		err := Validate(spec)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Validate(max-memory %q) = %v, want it to say %q", c.in, err, c.want)
		}
	}
}
