package process

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRotateLog(t *testing.T) {
	t.Run("missing file is nothing to rotate", func(t *testing.T) {
		rotated, err := RotateLog(filepath.Join(t.TempDir(), "nope.log"), 10)
		if err != nil || rotated {
			t.Errorf("RotateLog(missing) = (%v, %v), want (false, nil)", rotated, err)
		}
	})

	t.Run("under the limit is left alone", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.log")
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 100), 0644); err != nil {
			t.Fatal(err)
		}

		rotated, err := RotateLog(path, 100) // exactly at the limit counts as under
		if err != nil || rotated {
			t.Errorf("RotateLog(at limit) = (%v, %v), want (false, nil)", rotated, err)
		}
		if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
			t.Errorf("%s.1 exists after a no-op rotation", path)
		}
	})

	// The property the whole mechanism rests on: a writer holding the file
	// open O_APPEND — exactly how process.Start hands the log to the R
	// process — keeps working across the rotation, with its next write
	// starting the fresh log at offset zero rather than leaving a hole or
	// going to the rotated copy.
	t.Run("rotates under a live O_APPEND writer", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.log")
		w, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		old := bytes.Repeat([]byte("old line\n"), 20) // 180 bytes
		if _, err := w.Write(old); err != nil {
			t.Fatal(err)
		}

		rotated, err := RotateLog(path, 100)
		if err != nil {
			t.Fatal(err)
		}
		if !rotated {
			t.Fatal("RotateLog(over limit) = false, want true")
		}

		if got, _ := os.ReadFile(path + ".1"); !bytes.Equal(got, old) {
			t.Errorf("%s.1 has %d bytes, want the %d old bytes", path, len(got), len(old))
		}
		if info, _ := os.Stat(path); info.Size() != 0 {
			t.Errorf("live log is %d bytes after rotation, want 0", info.Size())
		}

		if _, err := w.Write([]byte("new line\n")); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "new line\n" {
			t.Errorf("live log after post-rotation write = %q, want just the new line (no hole, no old data)", got)
		}
	})

	t.Run("a second rotation replaces the previous generation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.log")
		for _, gen := range []string{"first", "second"} {
			if err := os.WriteFile(path, bytes.Repeat([]byte(gen+"\n"), 50), 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := RotateLog(path, 10); err != nil {
				t.Fatal(err)
			}
		}

		got, _ := os.ReadFile(path + ".1")
		if !bytes.HasPrefix(got, []byte("second\n")) {
			t.Errorf("%s.1 starts with %q, want the second generation", path, bytes.SplitN(got, []byte("\n"), 2)[0])
		}

		entries, _ := os.ReadDir(filepath.Dir(path))
		if len(entries) != 2 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("dir has %v, want only app.log and app.log.1 (no temp files left)", names)
		}
	})
}
