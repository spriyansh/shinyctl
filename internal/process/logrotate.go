package process

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MaxLogSize is the size a log file may reach before RotateLog turns it
// over: 1 GiB. Applies to each app's own log and to shinyctl-api's. One
// previous generation is kept (`<name>.1`), so the worst case on disk is
// twice this per log.
const MaxLogSize = 1 << 30

// RotateLog turns path over if it has grown past max: the current contents
// are copied to `path.1` (replacing whatever was there) and path itself is
// truncated to zero. It reports whether a rotation happened. A missing
// file is simply "nothing to rotate."
//
// Copy-then-truncate, rather than the usual rename-and-reopen, because
// the process writing the log isn't ours to signal: an R process holds
// its log open from exec until exit, and shinyctl-api can't hand it a new
// descriptor. Truncation works with that instead of against it — the
// child's descriptor is O_APPEND, and O_APPEND writes always land at the
// current end of file, so after the truncate its next line starts the
// fresh log at offset zero. The cost is the same one logrotate's
// `copytruncate` accepts: anything written in the gap between the copy
// finishing and the truncate is lost.
func RotateLog(path string, max int64) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.Size() <= max {
		return false, nil
	}

	// Copy into a temp sibling and rename over `.1`, so a crash mid-copy
	// never replaces a good previous generation with a partial one.
	prev := path + ".1"
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(prev)+".*.tmp")
	if err != nil {
		return false, fmt.Errorf("rotating %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename

	src, err := os.Open(path)
	if err != nil {
		tmp.Close()
		return false, fmt.Errorf("rotating %s: %w", path, err)
	}
	_, copyErr := io.Copy(tmp, src)
	src.Close()
	if closeErr := tmp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return false, fmt.Errorf("rotating %s: %w", path, copyErr)
	}

	if err := os.Rename(tmpPath, prev); err != nil {
		return false, fmt.Errorf("rotating %s: %w", path, err)
	}

	if err := os.Truncate(path, 0); err != nil {
		return false, fmt.Errorf("rotating %s: %w", path, err)
	}

	return true, nil
}
