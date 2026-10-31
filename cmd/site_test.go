/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The website in site/ is plain HTML with no build step; these tests do the
// checks a site generator would.

// googleVerification matches the file Google Search Console asks for to
// prove the site is ours: one line of text, not a page of the site.
var googleVerification = regexp.MustCompile(`^google[0-9a-f]+\.html$`)

func sitePages(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("../site/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no pages in ../site (%v)", err)
	}
	pages := map[string]string{}
	for _, f := range files {
		if googleVerification.MatchString(filepath.Base(f)) {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		pages[filepath.Base(f)] = string(data)
	}
	return pages
}

// TestSiteLinksResolve: every link, image, script and stylesheet a page
// names inside the site exists, and every #fragment names an id on the
// page it points to. So does every url() in the stylesheet.
func TestSiteLinksResolve(t *testing.T) {
	pages := sitePages(t)
	ref := regexp.MustCompile(`(?:href|src)="([^"]*)"`)
	for name, page := range pages {
		for _, m := range ref.FindAllStringSubmatch(page, -1) {
			target := m[1]
			if regexp.MustCompile(`^(https?:|mailto:|data:)`).MatchString(target) {
				continue
			}
			file, frag, _ := strings.Cut(target, "#")
			if file == "" {
				file = name
			}
			file = path.Clean(file)
			if strings.HasPrefix(file, "..") || strings.HasPrefix(file, "/") {
				t.Errorf("%s: %q leaves the site; links between pages are relative", name, target)
				continue
			}
			if _, err := os.Stat(filepath.Join("../site", file)); err != nil {
				t.Errorf("%s: %q names a file that doesn't exist", name, target)
				continue
			}
			if frag != "" && !strings.Contains(pages[file], `id="`+frag+`"`) {
				t.Errorf("%s: %q names an id that %s doesn't have", name, target, file)
			}
		}
	}
	css, err := os.ReadFile("../site/assets/site.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`url\("([^"]+)"\)`).FindAllStringSubmatch(string(css), -1) {
		if _, err := os.Stat(filepath.Join("../site/assets", m[1])); err != nil {
			t.Errorf("site.css: url(%q) names a file that doesn't exist", m[1])
		}
	}
}

// TestSiteHeaderIsTheSameOnEveryPage: the header, the footer and the docs
// sidebar are copied into every page; they must not drift apart, and each
// page marks its own link as the current one.
func TestSiteHeaderIsTheSameOnEveryPage(t *testing.T) {
	pages := sitePages(t)
	current := regexp.MustCompile(`<a href="([^"]+)" aria-current="page">`)
	parts := map[string]*regexp.Regexp{
		"header":  regexp.MustCompile(`(?s)<header class="top">.*?</header>`),
		"footer":  regexp.MustCompile(`(?s)<footer class="foot">.*?</footer>`),
		"sidebar": regexp.MustCompile(`(?s)<aside class="side">.*?</aside>`),
	}
	for part, re := range parts {
		var want, from string
		for name, page := range pages {
			got := re.FindString(page)
			if got == "" {
				if part != "sidebar" || name != "index.html" {
					t.Errorf("%s has no %s", name, part)
				}
				continue
			}
			for _, m := range current.FindAllStringSubmatch(got, -1) {
				if m[1] != name {
					t.Errorf("%s: its %s marks %s as the current page", name, part, m[1])
				}
			}
			got = strings.ReplaceAll(got, ` aria-current="page"`, "")
			if want == "" {
				want, from = got, name
			} else if got != want {
				t.Errorf("the %s of %s differs from the one in %s", part, name, from)
			}
		}
	}
}

// TestSiteHasNoProductName: like the README, the site describes a built-in
// load balancer and does not say what it is built on.
func TestSiteHasNoProductName(t *testing.T) {
	err := filepath.WalkDir("../site", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !regexp.MustCompile(`\.(html|css|js|svg)$`).MatchString(p) {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if loc := regexp.MustCompile(`(?i)caddy`).FindIndex(data); loc != nil {
			t.Errorf("%s names the software behind the load balancer at byte %d", p, loc[0])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSiteMatchesTheREADME: the site's install command is the README's,
// and every version it names is the release the README installs, so a new
// release can't leave the site behind.
func TestSiteMatchesTheREADME(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`releases/download/(v\d+\.\d+\.\d+)/`).FindSubmatch(readme)
	if m == nil {
		t.Fatal("README.md names no release to download")
	}
	release := string(m[1])
	const install = "curl -fsSL https://raw.githubusercontent.com/spriyansh/shinyctl/main/install.sh | sh"
	if !strings.Contains(string(readme), install) {
		t.Fatalf("README.md does not contain %q", install)
	}
	script, err := os.ReadFile("../site/assets/site.js")
	if err != nil {
		t.Fatal(err)
	}
	files := sitePages(t)
	files["assets/site.js"] = string(script)
	version := regexp.MustCompile(`\bv\d+\.\d+\.\d+\b`)
	for name, text := range files {
		if strings.Contains(text, "install.sh") && !strings.Contains(text, install) {
			t.Errorf("%s names install.sh but not the README's command %q", name, install)
		}
		for _, v := range version.FindAllString(text, -1) {
			if v != release {
				t.Errorf("%s names %s; the README installs %s", name, v, release)
			}
		}
	}
	if !strings.Contains(files["index.html"], install) {
		t.Errorf("index.html does not show the install command %q", install)
	}
}
