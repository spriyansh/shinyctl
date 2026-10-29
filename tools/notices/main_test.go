package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// moduleRoot is the directory of the module the tests run in.
func moduleRoot(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH")
	}
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestNotices_CoversEveryModule(t *testing.T) {
	root := moduleRoot(t)
	var buf bytes.Buffer
	if err := generate(&buf, root); err != nil {
		t.Fatal(err)
	}
	text := buf.String()

	mods, err := modules(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) < 50 {
		t.Fatalf("only %d modules found, the list looks wrong", len(mods))
	}
	for _, m := range mods {
		if !strings.Contains(text, "\n"+m.Path+" "+m.Version+"\n") {
			t.Errorf("%s %s is not in the notices", m.Path, m.Version)
		}
		// Every NOTICE file is reproduced (Apache-2.0 section 4(d)).
		entries, _ := os.ReadDir(m.Dir)
		for _, e := range entries {
			if strings.HasPrefix(strings.ToUpper(e.Name()), "NOTICE") && !e.IsDir() {
				data, _ := os.ReadFile(filepath.Join(m.Dir, e.Name()))
				first, _, _ := strings.Cut(strings.TrimSpace(string(data)), "\n")
				if !strings.Contains(text, first) {
					t.Errorf("%s: the first line of %s is missing: %q", m.Path, e.Name(), first)
				}
			}
		}
	}
	if !strings.Contains(text, "\ngithub.com/caddyserver/caddy/v2 v") {
		t.Error("the load balancer's engine is not in the notices")
	}
	if !strings.Contains(text, "Go (the standard library and runtime)") || !strings.Contains(text, "The Go Authors") {
		t.Error("Go's own license is not in the notices")
	}
	if !strings.HasPrefix(text, "shinyctl is MIT-licensed") {
		t.Error("the header is missing")
	}
}

func TestNotices_MissingLicenseIsAnError(t *testing.T) {
	goroot := t.TempDir()
	os.WriteFile(filepath.Join(goroot, "LICENSE"), []byte("Go license\n"), 0o644)
	withLicense := t.TempDir()
	os.WriteFile(filepath.Join(withLicense, "LICENSE"), []byte("MIT\n"), 0o644)
	without := t.TempDir()
	os.WriteFile(filepath.Join(without, "README.md"), []byte("no license here\n"), 0o644)

	var buf bytes.Buffer
	err := write(&buf, []module{{Path: "example.com/a", Version: "v1.0.0", Dir: withLicense}}, goroot)
	if err != nil {
		t.Fatalf("a module with a license: %v", err)
	}
	if !strings.Contains(buf.String(), "example.com/a v1.0.0") || !strings.Contains(buf.String(), "MIT") {
		t.Errorf("notices = %q, want the module and its license", buf.String())
	}

	err = write(&buf, []module{{Path: "example.com/a", Version: "v1.0.0", Dir: withLicense}, {Path: "example.com/b", Version: "v2.0.0", Dir: without}}, goroot)
	if err == nil || !strings.Contains(err.Error(), "example.com/b") {
		t.Errorf("error = %v, want one that names example.com/b", err)
	}
}

// The tool's targets are the systems the release is built for, read from
// .goreleaser.yaml, not from the tool itself.
func TestNotices_TargetsAreTheReleaseTargets(t *testing.T) {
	root := moduleRoot(t)
	conf, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	list := func(key string) []string {
		m := regexp.MustCompile(`(?m)^    ` + key + `:\n((?:      - \w+\n)+)`).FindSubmatch(conf)
		if m == nil {
			t.Fatalf(".goreleaser.yaml has no %s list", key)
		}
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(string(m[1])), "\n") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- ")))
		}
		return out
	}
	var want []string
	for _, goos := range list("goos") {
		for _, goarch := range list("goarch") {
			want = append(want, goos+"/"+goarch)
		}
	}
	var got []string
	for _, t := range targets {
		got = append(got, t[0]+"/"+t[1])
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the tool's targets are %v, .goreleaser.yaml builds %v", got, want)
	}
}

// Things that must be in the notices, named here and not taken from the
// tool's own list: a module only one system links, and the licenses of code
// vendored inside linked packages.
func TestNotices_HasWhatItMust(t *testing.T) {
	var buf bytes.Buffer
	if err := generate(&buf, moduleRoot(t)); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	for _, want := range []string{
		"\nhowett.net/plist v",                   // linked on darwin only
		"\ngithub.com/prometheus/procfs v",       // linked on linux only
		"\ngithub.com/go-jose/go-jose/v3 json\n", // vendored, with its own license
		"\ngithub.com/go-jose/go-jose/v4 json\n",
		"\ngithub.com/dgraph-io/ristretto z\n",
		"\ngithub.com/klauspost/compress internal/snapref\n",
		"\ngithub.com/klauspost/compress zstd/internal/xxhash\n",
		"\ngithub.com/smallstep/cli-utils pkg/blackfriday\n",
		"\ngithub.com/smallstep/pkcs7 internal/cryptobyte\n",
		"\ngo.step.sm/crypto internal/bcrypt_pbkdf\n",
		"\ngo.step.sm/crypto internal/utils/utfbom\n",
		"\ngoogle.golang.org/api internal/third_party/uritemplates\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the notices do not have %q", strings.TrimSpace(want))
		}
	}
	// A module whose COPYRIGHT file names a holder its LICENSE does not.
	if i := strings.Index(text, "\ngithub.com/miekg/dns v"); i < 0 || !strings.Contains(text[i:i+8000], "--- COPYRIGHT ---") {
		t.Error("the notices of miekg/dns lack its COPYRIGHT file")
	}
	// Both files of a directory that has a license and a patent grant.
	i := strings.Index(text, "\ngithub.com/smallstep/pkcs7 internal/cryptobyte\n")
	if i < 0 || !strings.Contains(text[i:i+4000], "--- PATENTS ---") || !strings.Contains(text[i:i+4000], "--- LICENSE ---") {
		t.Error("the notices of smallstep/pkcs7 internal/cryptobyte lack its LICENSE or PATENTS")
	}
}

// A license file in the directory of a linked package, or in a parent below
// the module's root, is a section of its own, labelled by module and path.
func TestNotices_SubdirectoryLicenses(t *testing.T) {
	goroot := t.TempDir()
	os.WriteFile(filepath.Join(goroot, "LICENSE"), []byte("Go license\n"), 0o644)
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "LICENSE"), []byte("root license\n"), 0o644)
	deep := filepath.Join(root, "internal", "vendored", "pkg")
	os.MkdirAll(deep, 0o755)
	os.WriteFile(filepath.Join(root, "internal", "vendored", "LICENSE.txt"), []byte("parent license\n"), 0o644)
	os.WriteFile(filepath.Join(deep, "COPYING"), []byte("package license\n"), 0o644)
	os.WriteFile(filepath.Join(deep, "NOTICE"), []byte("package notice\n"), 0o644)
	plain := filepath.Join(root, "plain")
	os.MkdirAll(plain, 0o755)

	var buf bytes.Buffer
	m := module{Path: "example.com/m", Version: "v1.0.0", Dir: root, PkgDirs: []string{plain, deep, root}}
	if err := write(&buf, []module{m}, goroot); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	for _, want := range []string{
		"\nexample.com/m v1.0.0\n", "root license",
		"\nexample.com/m internal/vendored\n", "parent license",
		"\nexample.com/m internal/vendored/pkg\n", "package license", "package notice",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the notices do not have %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "example.com/m plain") {
		t.Error("a directory without a license got a section")
	}
}
