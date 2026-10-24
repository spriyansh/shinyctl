package proxy

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestLoadSecret(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadSecret(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(first) {
		t.Errorf("secret = %q, want 64 hex characters", first)
	}
	info, err := os.Stat(filepath.Join(dir, "secret"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("secret file: %v, %v; want mode 0600", info, err)
	}

	second, err := LoadSecret(dir)
	if err != nil || second != first {
		t.Errorf("second call = %q, %v; want the same secret", second, err)
	}
}

// A key that can't be used is replaced with one line in the log, atomically;
// shinyctl-api must still start.
func TestLoadSecret_UnusableIsReplaced(t *testing.T) {
	good := strings.Repeat("ab", 32)
	cases := map[string]func(t *testing.T, path string){
		"empty":    func(t *testing.T, path string) { os.WriteFile(path, nil, 0o600) },
		"newline":  func(t *testing.T, path string) { os.WriteFile(path, []byte("\n"), 0o600) },
		"short":    func(t *testing.T, path string) { os.WriteFile(path, []byte("abcd\n"), 0o600) },
		"non-hex":  func(t *testing.T, path string) { os.WriteFile(path, []byte(strings.Repeat("zz", 32)), 0o600) },
		"too long": func(t *testing.T, path string) { os.WriteFile(path, []byte(good+"ab"), 0o600) },
		"unreadable": func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root reads any file")
			}
			os.WriteFile(path, []byte(good), 0o600)
			os.Chmod(path, 0)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "secret")
			setup(t, path)

			var logs syncBuffer
			log.SetOutput(&logs)
			defer log.SetOutput(os.Stderr)

			got, err := LoadSecret(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got) || got == good {
				t.Errorf("secret = %q, want a new key", got)
			}
			want := "the key for the load balancer's cookies was unusable ("
			if strings.Count(logs.String(), want) != 1 || !strings.Contains(logs.String(), "made a new one; visitors pick a copy again once") {
				t.Errorf("log = %q, want one line saying the key was unusable", logs.String())
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Errorf("secret file: %v, %v; want mode 0600", info, err)
			}
			if again, err := LoadSecret(dir); err != nil || again != got {
				t.Errorf("second call = %q, %v; want the new key kept", again, err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Errorf("directory holds %d files, want only secret (no temp file left)", len(entries))
			}
		})
	}
}

// A fresh install has nothing to report; a key that can't be replaced is an
// error that says what is wrong and not a log line claiming a new key.
func TestLoadSecret_FreshInstallIsSilent(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	if _, err := LoadSecret(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if logs.String() != "" {
		t.Errorf("a fresh install logged %q", logs.String())
	}
}

func TestLoadSecret_UnusableAndNotReplaceable(t *testing.T) {
	var logs syncBuffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "secret"), 0o700) // a directory where the key should be
	_, err := LoadSecret(dir)
	if err == nil || !strings.Contains(err.Error(), "is unusable") || !strings.Contains(err.Error(), "a new key cannot be written") {
		t.Errorf("err = %v, want it to say the key is unusable and cannot be replaced", err)
	}
	if strings.Contains(logs.String(), "made a new one") {
		t.Errorf("the log claims a new key although none was written: %q", logs.String())
	}

	if os.Geteuid() != 0 {
		ro := t.TempDir()
		os.WriteFile(filepath.Join(ro, "secret"), []byte("junk"), 0o600)
		os.Chmod(ro, 0o500)
		defer os.Chmod(ro, 0o700)
		if _, err := LoadSecret(ro); err == nil || !strings.Contains(err.Error(), "a new key cannot be written") {
			t.Errorf("a read-only directory: err = %v", err)
		}
	}
}

func TestLoadSecret_CannotBeWritten(t *testing.T) {
	if _, err := LoadSecret(filepath.Join(t.TempDir(), "missing", "dir")); err == nil {
		t.Error("want an error when the file cannot be created")
	}
}
