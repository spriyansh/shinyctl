// Command notices writes the license notices of the software that a shinyctl
// release contains: Go's own license and, for every module the binary is
// built from, the license and notice files from the module's root and from
// the directory of each linked package and its parents up to the root (some
// modules carry code of others in a sub-directory, with its own license).
//
//	go run ./tools/notices -o build/THIRD_PARTY_NOTICES
//
// The release build runs it and puts the file in each archive (see
// .goreleaser.yaml). It fails, naming the module, when a module has no
// license file, so a new dependency without one is found by CI.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// targets are the systems the releases are built for; a module only one of
// them uses is listed too.
var targets = [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}}

// licenseFile matches the files that carry a module's license, notices and
// patent grants. Apache-2.0 section 4(d) requires a NOTICE file to be
// reproduced.
var licenseFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|copyright|notice|patents)(\.(md|txt))?$`)

const header = `shinyctl is MIT-licensed (see LICENSE). This program also contains the
software listed below, under the licenses reproduced here. Its built-in
load balancer is built on Caddy (https://caddyserver.com), Apache License 2.0.
`

type module struct {
	Path, Version, Dir string
	PkgDirs            []string // directories of the linked packages, sorted
}

func main() {
	out := flag.String("o", "", "the file to write (default: standard output)")
	flag.Parse()

	var w io.Writer = os.Stdout
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			fatal(err)
		}
		f, err := os.Create(*out)
		if err != nil {
			fatal(err)
		}
		defer f.Close()
		w = f
	}
	if err := generate(w, "."); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "notices:", err)
	os.Exit(1)
}

// generate writes the notices for the module in dir.
func generate(w io.Writer, dir string) error {
	mods, err := modules(dir)
	if err != nil {
		return err
	}
	goroot, err := goRoot(dir)
	if err != nil {
		return err
	}
	return write(w, mods, goroot)
}

func write(w io.Writer, mods []module, goroot string) error {
	var buf bytes.Buffer
	buf.WriteString(header)
	if err := section(&buf, "Go (the standard library and runtime)", goroot, false); err != nil {
		return err
	}
	for _, m := range mods {
		if err := section(&buf, m.Path+" "+m.Version, m.Dir, true); err != nil {
			return fmt.Errorf("%s: %w", m.Path, err)
		}
		for _, rel := range m.subDirs() {
			if err := section(&buf, m.Path+" "+rel, filepath.Join(m.Dir, filepath.FromSlash(rel)), false); err != nil {
				return fmt.Errorf("%s %s: %w", m.Path, rel, err)
			}
		}
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// subDirs lists, sorted, the directories below the module's root that hold a
// license file and are the directory of a linked package or a parent of one
// (slash-separated, relative to the root).
func (m module) subDirs() []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range m.PkgDirs {
		for d != m.Dir && strings.HasPrefix(d, m.Dir+string(filepath.Separator)) {
			rel := filepath.ToSlash(strings.TrimPrefix(d, m.Dir+string(filepath.Separator)))
			if !seen[rel] {
				seen[rel] = true
				if len(licenseFiles(d)) > 0 {
					out = append(out, rel)
				}
			}
			d = filepath.Dir(d)
		}
	}
	sort.Strings(out)
	return out
}

// licenseFiles lists, sorted, the license, notice and patent files in dir.
func licenseFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && licenseFile.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// section writes a rule, a title and the license files of dir. It is an error
// when there is none and required is set.
func section(buf *bytes.Buffer, title, dir string, required bool) error {
	if _, err := os.ReadDir(dir); err != nil {
		return err
	}
	names := licenseFiles(dir)
	if len(names) == 0 && required {
		return fmt.Errorf("no license file in %s", dir)
	}
	fmt.Fprintf(buf, "\n%s\n%s\n", strings.Repeat("=", 78), title)
	for _, name := range names {
		text, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(buf, "\n--- %s ---\n\n%s", name, text)
		if !bytes.HasSuffix(text, []byte("\n")) {
			buf.WriteByte('\n')
		}
	}
	return nil
}

// modules lists, sorted by path, the modules other than the main one that the
// packages of dir depend on, for any of the targets.
func modules(dir string) ([]module, error) {
	seen := map[string]*module{}
	dirs := map[string]map[string]bool{}
	for _, t := range targets {
		cmd := exec.Command("go", "list", "-deps", "-f",
			`{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}{{"\t"}}{{.Dir}}{{"\t"}}{{$.Dir}}{{end}}{{end}}`, ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOOS="+t[0], "GOARCH="+t[1], "CGO_ENABLED=0")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list for %s/%s: %v: %s", t[0], t[1], err, stderr.String())
		}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Split(line, "\t")
			if len(f) == 4 && f[0] != "" {
				seen[f[0]] = &module{Path: f[0], Version: f[1], Dir: f[2]}
				if dirs[f[0]] == nil {
					dirs[f[0]] = map[string]bool{}
				}
				dirs[f[0]][f[3]] = true
			}
		}
	}
	list := make([]module, 0, len(seen))
	for path, m := range seen {
		for d := range dirs[path] {
			m.PkgDirs = append(m.PkgDirs, d)
		}
		sort.Strings(m.PkgDirs)
		list = append(list, *m)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	return list, nil
}

func goRoot(dir string) (string, error) {
	cmd := exec.Command("go", "env", "GOROOT")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOROOT: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
