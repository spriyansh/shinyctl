package logtail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spriyansh/shinyctl/internal/process"
)

// lockedBuffer lets a test read what Follow has written so far while
// Follow's goroutine is still writing.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls until the buffer holds exactly want. Follow polls on its
// own interval, so tests wait for the output instead of sleeping.
func waitFor(t *testing.T, b *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for b.String() != want {
		if time.Now().After(deadline) {
			t.Fatalf("output = %q, want %q", b.String(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// countingReaderAt records how many bytes were read through it.
type countingReaderAt struct {
	r    *strings.Reader
	read int
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.read += n
	return n, err
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func follow(ctx context.Context, path string, offset int64, w *lockedBuffer) chan error {
	done := make(chan error, 1)
	go func() { done <- Follow(ctx, path, offset, w, 5*time.Millisecond) }()
	return done
}

func TestLastLinesStart(t *testing.T) {
	cases := []struct {
		name string
		data string
		n    int
		want string
	}{
		{"empty file", "", 3, ""},
		{"zero lines", "a\nb\nc\n", 0, ""},
		{"last line", "a\nb\nc\n", 1, "c\n"},
		{"last two", "a\nb\nc\n", 2, "b\nc\n"},
		{"exactly all", "a\nb\nc\n", 3, "a\nb\nc\n"},
		{"more than there are", "a\nb\nc\n", 10, "a\nb\nc\n"},
		{"no trailing newline, one", "a\nb\nc", 1, "c"},
		{"no trailing newline, two", "a\nb\nc", 2, "b\nc"},
		{"blank lines count", "a\n\n\nb\n", 2, "\nb\n"},
		{"only a newline", "\n", 1, "\n"},
	}

	for _, c := range cases {
		for _, block := range []int{1, 2, 3, 7, 64 << 10} {
			t.Run(fmt.Sprintf("%s/block %d", c.name, block), func(t *testing.T) {
				size := int64(len(c.data))
				start, err := lastLinesStart(strings.NewReader(c.data), size, c.n, block)
				if err != nil {
					t.Fatal(err)
				}
				if got := c.data[start:]; got != c.want {
					t.Errorf("last %d lines = %q, want %q", c.n, got, c.want)
				}
			})
		}
	}
}

func TestLastLinesStart_ReadsOnlyTheEnd(t *testing.T) {
	var sb strings.Builder
	for i := 0; sb.Len() < 8<<20; i++ {
		fmt.Fprintf(&sb, "line %07d\n", i)
	}
	data := sb.String()

	r := &countingReaderAt{r: strings.NewReader(data)}
	start, err := lastLinesStart(r, int64(len(data)), 10, 64<<10)
	if err != nil {
		t.Fatal(err)
	}

	if r.read > 64<<10 {
		t.Errorf("read %d bytes of a %d byte log, want at most one 64 KiB block", r.read, len(data))
	}

	lines := strings.SplitAfter(data, "\n")
	want := strings.Join(lines[len(lines)-11:], "") // SplitAfter leaves a trailing ""
	if got := data[start:]; got != want {
		t.Errorf("last 10 lines = %q, want %q", got, want)
	}
}

func TestLast(t *testing.T) {
	t.Run("prints the end and returns the size", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.log")
		data := "one\ntwo\nthree\nfour\nfive\n"
		if err := os.WriteFile(path, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer
		offset, err := Last(path, 2, &out)
		if err != nil {
			t.Fatal(err)
		}

		if out.String() != "four\nfive\n" {
			t.Errorf("output = %q, want %q", out.String(), "four\nfive\n")
		}
		if offset != int64(len(data)) {
			t.Errorf("offset = %d, want %d", offset, len(data))
		}
	})

	t.Run("zero lines prints nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.log")
		if err := os.WriteFile(path, []byte("a\nb\n"), 0644); err != nil {
			t.Fatal(err)
		}

		var out bytes.Buffer
		offset, err := Last(path, 0, &out)
		if err != nil || out.Len() != 0 || offset != 4 {
			t.Errorf("Last(n=0) = (%d, %v) with output %q, want (4, nil) and no output", offset, err, out.String())
		}
	})

	t.Run("missing file", func(t *testing.T) {
		_, err := Last(filepath.Join(t.TempDir(), "nope.log"), 5, &bytes.Buffer{})
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Last(missing) error = %v, want one wrapping fs.ErrNotExist", err)
		}
	})
}

func TestFollow_PrintsAppendedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "before\n")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var out lockedBuffer
	done := follow(ctx, path, int64(len("before\n")), &out)

	appendTo(t, path, "first\n")
	waitFor(t, &out, "first\n")
	appendTo(t, path, "second\nthird\n")
	waitFor(t, &out, "first\nsecond\nthird\n")

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Follow() = %v, want nil after cancel", err)
	}
}

// Rotation here is the real process.RotateLog, so this breaks if its
// copy-then-truncate ever changes shape.
func TestFollow_StartsOverAfterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	old := strings.Repeat("old line\n", 20) // 180 bytes
	appendTo(t, path, old)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var out lockedBuffer
	done := follow(ctx, path, int64(len(old)), &out)

	rotated, err := process.RotateLog(path, 100)
	if err != nil || !rotated {
		t.Fatalf("RotateLog() = (%v, %v), want (true, nil)", rotated, err)
	}
	appendTo(t, path, "new line\n")

	waitFor(t, &out, "new line\n")

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Follow() = %v, want nil after cancel", err)
	}
}

func TestFollow_FollowsReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "old content that is longer than the new\n")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var out lockedBuffer
	done := follow(ctx, path, 0, &out)

	// Wait until Follow has the old file open before replacing it, or it
	// could open the path in the gap between the remove and the write.
	waitFor(t, &out, "old content that is longer than the new\n")

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fresh\n"), 0644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, &out, "old content that is longer than the new\nfresh\n")

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Follow() = %v, want nil after cancel", err)
	}
}

// If the replacement can't be opened (the path vanished again between the
// stat and the open), Follow keeps reading the old file instead of failing,
// and switches once the open works.
func TestFollow_KeepsOldFileWhenReopenFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	appendTo(t, path, "old\n")

	// A handle of our own on the old file, to append to it after the path
	// no longer names it.
	oldFile, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close()

	var failing atomic.Bool
	failing.Store(true)
	attempted := make(chan struct{}, 1)
	realOpen := openFile
	openFile = func(name string) (*os.File, error) {
		if failing.Load() {
			select {
			case attempted <- struct{}{}:
			default:
			}
			return nil, fs.ErrNotExist
		}
		return realOpen(name)
	}
	t.Cleanup(func() { openFile = realOpen })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var out lockedBuffer
	done := follow(ctx, path, 0, &out)
	waitFor(t, &out, "old\n")

	next := filepath.Join(dir, "next.log")
	if err := os.WriteFile(next, []byte("fresh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}

	select {
	case <-attempted:
	case <-time.After(5 * time.Second):
		t.Fatal("Follow never tried to reopen the replaced file")
	}
	if _, err := oldFile.WriteString("late\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, &out, "old\nlate\n")

	failing.Store(false)
	waitFor(t, &out, "old\nlate\nfresh\n")

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Follow() = %v, want nil after cancel", err)
	}
}

func TestFollow_ReturnsNilWhenCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	appendTo(t, path, "x\n")

	ctx, cancel := context.WithCancel(t.Context())
	var out lockedBuffer
	done := follow(ctx, path, 2, &out)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Follow() = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Follow() did not return after cancel")
	}
}

func TestFollow_MissingFile(t *testing.T) {
	err := Follow(t.Context(), filepath.Join(t.TempDir(), "nope.log"), 0, &bytes.Buffer{}, time.Millisecond)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Follow(missing) = %v, want one wrapping fs.ErrNotExist", err)
	}
}
