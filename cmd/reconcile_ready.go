/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"sync"
	"syscall"
	"time"

	"github.com/spriyansh/shinyctl/internal/process"
)

// readyWatched holds the PIDs of the copies a ready-watcher is already
// following, so a copy gets one however many cycles look at it.
var readyWatched sync.Map

// readyPoll is how often a ready-watcher looks, for the first half minute
// (an R process with a slow startup is followed more gently after that).
var readyPoll = 100 * time.Millisecond

// readyPollSlow is the interval after readySlowAfter.
const (
	readyPollSlow  = time.Second
	readySlowAfter = 30 * time.Second
)

// watchReady triggers a reconcile of the app as soon as the copy at pid
// accepts connections on port, so it can be given a role and serve visitors;
// it gives up when the process is gone. A copy takes the role of an active
// copy or a warm one only once it is ready, and nothing else notices that
// moment: the periodic tick would take up to five seconds.
func watchReady(stateFile string, pid, port int) {
	if _, loaded := readyWatched.LoadOrStore(pid, struct{}{}); loaded {
		return
	}

	start := time.Now()
	for {
		// Gone, or the pid is another user's process now (EPERM): nothing to
		// wait for.
		if err := currentHooks().Signal0(pid); err == syscall.ESRCH || err == syscall.EPERM {
			readyWatched.Delete(pid)
			return
		}
		if currentHooks().WatchProbe(process.InternalHost, port) {
			// The slot is freed first: if the cycle below finds the copy not
			// ready after all (one probe of its own failed), it starts a new
			// watcher, which must not find this one still registered.
			readyWatched.Delete(pid)
			triggerReconcile(stateFile)
			return
		}
		if time.Since(start) < readySlowAfter {
			time.Sleep(readyPoll)
		} else {
			time.Sleep(readyPollSlow)
		}
	}
}
