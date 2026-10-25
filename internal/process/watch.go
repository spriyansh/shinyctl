package process

import (
	"errors"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/state"
)

// ErrNotOurs is what WatchExit returns for a process that is already gone or
// is not the one app records.
var ErrNotOurs = errors.New("the process is gone or is not the app's")

// ErrWatchUnsupported is what WatchExit returns where the system has no way to
// watch a process that is not our child. Other errors come from the system
// refusing (an old Linux kernel, a sandbox).
var ErrWatchUnsupported = errors.New("watching a process for its exit is not supported here")

// exitHandle is a handle on one process that becomes ready when it exits.
type exitHandle interface {
	wait()
	close()
}

// WatchExit returns a channel that is closed when app's recorded process
// exits. It works for any process, not only our children: it is how a
// process left behind by an earlier run is noticed the instant it dies.
// It costs one goroutine and one descriptor until the process exits, and
// both are released then.
//
// The handle is opened before the identity is checked, not after. A handle
// names one process, whichever one held the PID when it was opened; it never
// moves to a later process that reuses the PID. So if the process the handle
// names is the app's (checked next, as IsAlive does), the exit it reports is
// the app's, and if the PID was reused before the handle was opened the check
// fails and the handle is closed. Checking first would leave a gap in which
// the app could die and its PID be taken before the handle was opened.
//
// An error means nothing is being watched: ErrNotOurs, ErrWatchUnsupported or
// whatever the system said; the caller falls back to checking regularly.
func WatchExit(spec manifest.AppSpec, app state.AppState) (<-chan struct{}, error) {
	if app.PID <= 0 {
		return nil, ErrNotOurs
	}
	h, err := openExitHandle(app.PID)
	if err != nil {
		return nil, err
	}
	if !IsAlive(spec, app) {
		h.close()
		return nil, ErrNotOurs
	}
	done := make(chan struct{})
	go func() {
		h.wait()
		h.close()
		close(done)
	}()
	return done, nil
}
