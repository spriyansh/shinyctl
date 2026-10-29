/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// fakeRelease serves what install.sh downloads from a releases page: a
// redirect from /latest to the tag, the archive for this system and
// checksums.txt. The archive's shinyctl is a script that prints the version.
// checksum replaces the archive's line in checksums.txt when it is not empty.
func fakeRelease(t *testing.T, version, checksum string) (url, archive string, latestCalls *int) {
	t.Helper()
	conf, err := os.ReadFile("../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*name_template: "(shinyctl_[^"]*)"`).FindSubmatch(conf)
	if m == nil {
		t.Fatal(".goreleaser.yaml has no archive name_template")
	}
	name := strings.NewReplacer("{{ .Version }}", strings.TrimPrefix(version, "v"), "{{ .Os }}", runtime.GOOS, "{{ .Arch }}", runtime.GOARCH).Replace(string(m[1]))

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	bin := fmt.Sprintf("#!/bin/sh\necho %s\n", version)
	tw.WriteHeader(&tar.Header{Name: name + "/shinyctl", Mode: 0o755, Size: int64(len(bin))})
	tw.Write([]byte(bin))
	tw.Close()
	gz.Close()
	data := buf.Bytes()
	if checksum == "" {
		checksum = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	sums := fmt.Sprintf("%064x  shinyctl_other.tar.gz\n%s  %s.tar.gz\n", 0, checksum, name)

	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Redirect(w, r, "/tag/"+version, http.StatusFound)
	})
	mux.HandleFunc("/tag/", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/download/"+version+"/"+name+".tar.gz", func(w http.ResponseWriter, r *http.Request) { w.Write(data) })
	mux.HandleFunc("/download/"+version+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, sums) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, name, &calls
}

// runInstall runs install.sh with a fresh HOME and the given settings.
func runInstall(t *testing.T, env ...string) (out, home string, err error) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("install.sh is tested with curl, and there is none")
	}
	home = t.TempDir()
	cmd := exec.Command("sh", "../install.sh")
	cmd.Env = append([]string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}, env...)
	b, err := cmd.CombinedOutput()
	return string(b), home, err
}

// TestInstallScript_InstallsTheLatestRelease: with no settings it asks the
// releases page for the latest version, checks the archive for this system
// and puts shinyctl in ~/.local/bin, saying that the folder is not on PATH.
func TestInstallScript_InstallsTheLatestRelease(t *testing.T) {
	url, _, latest := fakeRelease(t, "v9.8.7", "")
	out, home, err := runInstall(t, "SHINYCTL_RELEASES="+url)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	bin := filepath.Join(home, ".local", "bin", "shinyctl")
	if info, err := os.Stat(bin); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("no executable shinyctl at %s (%v):\n%s", bin, err, out)
	}
	if want := "Installed shinyctl v9.8.7 to " + bin; !strings.Contains(out, want) {
		t.Errorf("output does not say %q:\n%s", want, out)
	}
	if !strings.Contains(out, "is not on your PATH") {
		t.Errorf("output does not say the folder is not on PATH:\n%s", out)
	}
	if *latest != 1 {
		t.Errorf("/latest was asked %d times, want once", *latest)
	}
	if entries, _ := os.ReadDir(filepath.Dir(bin)); len(entries) != 1 {
		t.Errorf("the install folder has %d entries, want shinyctl alone", len(entries))
	}
}

// TestInstallScript_GivenVersionAndFolder: SHINYCTL_VERSION, with or
// without its v, skips the question for the latest; SHINYCTL_INSTALL_DIR is
// where the binary goes, and a folder on PATH gets no advice about PATH.
func TestInstallScript_GivenVersionAndFolder(t *testing.T) {
	url, _, latest := fakeRelease(t, "v9.8.7", "")
	dir := filepath.Join(t.TempDir(), "bin")
	out, _, err := runInstall(t, "SHINYCTL_RELEASES="+url, "SHINYCTL_VERSION=9.8.7", "SHINYCTL_INSTALL_DIR="+dir)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if got, err := exec.Command(filepath.Join(dir, "shinyctl"), "version").Output(); err != nil || strings.TrimSpace(string(got)) != "v9.8.7" {
		t.Errorf("the installed shinyctl says %q (%v), want v9.8.7", got, err)
	}
	if *latest != 0 {
		t.Errorf("/latest was asked %d times, want none when the version is given", *latest)
	}

	// Again, over the first one, with the folder on PATH.
	cmd := exec.Command("sh", "../install.sh")
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + dir + ":" + os.Getenv("PATH"),
		"SHINYCTL_RELEASES=" + url, "SHINYCTL_VERSION=v9.8.7", "SHINYCTL_INSTALL_DIR=" + dir}
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh over an installed shinyctl: %v\n%s", err, b)
	}
	if strings.Contains(string(b), "PATH") {
		t.Errorf("output has advice about PATH although the folder is on it:\n%s", b)
	}
}

// TestInstallScript_RefusesAnArchiveThatDoesNotMatch: a download that does
// not match checksums.txt is not installed.
func TestInstallScript_RefusesAnArchiveThatDoesNotMatch(t *testing.T) {
	url, _, _ := fakeRelease(t, "v9.8.7", strings.Repeat("ab", 32))
	out, home, err := runInstall(t, "SHINYCTL_RELEASES="+url)
	if err == nil {
		t.Fatalf("install.sh succeeded with a wrong checksum:\n%s", out)
	}
	if !strings.Contains(out, "does not match checksums.txt") {
		t.Errorf("output does not say the download does not match:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "shinyctl")); err == nil {
		t.Error("shinyctl was installed from an archive that does not match")
	}
}

// TestREADMEShowsTheInstallScript: the README's install section starts from
// the script on main.
func TestREADMEShowsTheInstallScript(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	want := "curl -fsSL https://raw.githubusercontent.com/spriyansh/shinyctl/main/install.sh | sh"
	if !strings.Contains(string(readme), want) {
		t.Errorf("README.md does not contain %q", want)
	}
}
