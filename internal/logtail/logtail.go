// Package logtail prints the end of a log file and follows it as it grows,
// including across process.RotateLog's copy-then-truncate.
package logtail

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"
)

// blockSize is how much of the file's end Last reads per step while it
// looks for the start of the last n lines: big enough that a typical tail
// is one read, small enough that a multi-GiB log is never loaded whole.
const blockSize = 64 << 10

// Last writes the last n lines of the file at path to w and returns the
// size it read up to. Hand that to Follow as its offset, so lines written
// between the two calls are printed once rather than twice or never.
// n <= 0 writes nothing.
func Last(path string, n int, w io.Writer) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()

	start, err := lastLinesStart(f, size, n, blockSize)
	if err != nil {
		return 0, err
	}

	if _, err := io.Copy(w, io.NewSectionReader(f, start, size-start)); err != nil {
		return 0, err
	}
	return size, nil
}

// lastLinesStart returns the offset in the first size bytes of r where the
// last n lines begin, scanning backwards in blocks so only the end of the
// file is read. A final newline ends the last line rather than starting an
// empty one; a last line without one still counts. Fewer than n lines
// gives 0, and n <= 0 gives size (nothing to print).
func lastLinesStart(r io.ReaderAt, size int64, n int, block int) (int64, error) {
	if n <= 0 {
		return size, nil
	}

	buf := make([]byte, block)
	seen := 0
	for end := size; end > 0; {
		start := end - int64(block)
		if start < 0 {
			start = 0
		}
		chunk := buf[:end-start]

		// A short read means the file shrank under us (rotation); better
		// to say so than to count newlines in a half-zeroed buffer.
		if read, err := r.ReadAt(chunk, start); read < len(chunk) {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return 0, fmt.Errorf("reading log: %w", err)
		}

		for i := len(chunk) - 1; i >= 0; i-- {
			at := start + int64(i)
			if chunk[i] != '\n' || at == size-1 {
				continue
			}
			seen++
			if seen == n {
				return at + 1, nil
			}
		}
		end = start
	}
	return 0, nil
}

// openFile is os.Open, as a package var so a test can make reopening a
// replaced log fail.
var openFile = os.Open

// Follow prints what is appended to the file at path, starting at offset,
// until ctx is cancelled (which returns nil: stopping is the normal way
// out). It polls every interval instead of using inotify/kqueue, which
// would be two code paths for a log that gets a few lines a second.
//
// Two ways the file can change under it are handled. process.RotateLog
// copies the log aside and truncates it in place, so the size drops below
// our offset and the new log starts at the top: we start over from 0.
// "Shorter than offset" is a reliable signal because rotation only happens
// past 1 GiB and a log can't regrow past the old offset within one poll.
// And if the path is removed and recreated, we reopen it; while it is
// missing we keep reading the old file, which is the best we can do.
func Follow(ctx context.Context, path string, offset int64, w io.Writer, interval time.Duration) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { f.Close() }()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() < offset {
			offset = 0
		}
		if info.Size() > offset {
			n, err := io.Copy(w, io.NewSectionReader(f, offset, info.Size()-offset))
			offset += n
			if err != nil {
				return err
			}
		}

		// Open the replacement before letting go of the old file: if the
		// path vanishes again in between, we keep following the old one
		// and try again on the next pass.
		if now, err := os.Stat(path); err == nil && !os.SameFile(info, now) {
			if nf, err := openFile(path); err == nil {
				f.Close()
				f, offset = nf, 0
				continue
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
