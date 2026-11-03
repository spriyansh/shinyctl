/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/state"
)

// readyWatched holds the copies, by pid and port, that a ready-watcher is
// already following, so a copy gets one however many cycles look at it. The
// port is part of the key: a pid the system hands to a new copy on another
// port must not find the old copy's watcher in its place.
var readyWatched sync.Map // pidPort -> struct{}

// readyPoll is how often a ready-watcher looks for the port, for the first
// half minute (an R process with a slow startup is followed more gently after
// that).
var readyPoll = 100 * time.Millisecond

// readyPollSlow is the interval after readySlowAfter.
const (
	readyPollSlow  = time.Second
	readySlowAfter = 30 * time.Second
)

// pageRetry and pageRetryMax are the real hooks.PageRetry and
// hooks.PageRetryMax: a failed page is asked again after pageRetry, doubled
// after each further failure up to pageRetryMax. Every failed page writes
// Shiny's error and its trace to the copy's log.
const (
	pageRetry    = time.Second
	pageRetryMax = 30 * time.Second
)

// pageChecks holds, by process, what is known of the copy's page: that it
// has answered, or what it does instead. Readiness is decided once per
// process and kept: R does one thing at a time, so a copy busy with a long
// computation answers no page for that long, and must not lose its visitors
// for it. A launch on a pid and port begins without an entry; the copy's
// watcher drops it when the process is gone.
var pageChecks sync.Map // pidPort -> pageState

// pageState is what is known of one process's page.
type pageState struct {
	ready        bool      // it has answered: the copy is ready for as long as the process lives
	problem      string    // what the last answer was instead, read after "its page"
	waitingSince time.Time // when the request still unanswered after PageSlow was sent; zero otherwise
}

func markPageReady(rec state.AppState) {
	pageChecks.Store(pidPort{rec.PID, rec.Port}, pageState{ready: true})
}

func forgetPage(rec state.AppState) { pageChecks.Delete(pidPort{rec.PID, rec.Port}) }

func pageOf(rec state.AppState) pageState {
	s, _ := pageChecks.Load(pidPort{rec.PID, rec.Port})
	p, _ := s.(pageState)
	return p
}

// pageReady says the copy's page has answered, for this process.
func pageReady(rec state.AppState) bool { return pageOf(rec).ready }

// setPageProblem and setPageWaiting are for the copy's own watcher only,
// which calls them only while the page has not answered.
func setPageProblem(rec state.AppState, problem string) {
	pageChecks.Store(pidPort{rec.PID, rec.Port}, pageState{problem: problem})
}

func setPageWaiting(rec state.AppState, asked time.Time) {
	s := pageOf(rec)
	s.waitingSince = asked
	pageChecks.Store(pidPort{rec.PID, rec.Port}, s)
}

// watchReady follows one copy until it is ready, then triggers a reconcile
// so it gets a role at once (the tick would take up to five seconds); it
// gives up when the process is gone. First the port, dialled as before; when
// it opens, one cycle runs at once, so the owner check moves a port another
// program holds before anything is said about a page. Then the page, unless
// it has answered for this process already (a probe of the cycle's own
// missed and brought the watcher back): then only the port was waited for,
// and the page is never asked again.
func watchReady(spec manifest.AppSpec, rec state.AppState) {
	key := pidPort{rec.PID, rec.Port}
	if _, loaded := readyWatched.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	// The process exited, or the pid is another user's process now (EPERM):
	// nothing to wait for, and nothing to remember about its page.
	gone := func() bool {
		if err := currentHooks().Signal0(rec.PID); err == syscall.ESRCH || err == syscall.EPERM {
			readyWatched.Delete(key)
			forgetPage(rec)
			return true
		}
		return false
	}

	start := time.Now()
	for {
		if gone() {
			return
		}
		if process.Listening(process.InternalHost, rec.Port) {
			break
		}
		if time.Since(start) < readySlowAfter {
			time.Sleep(readyPoll)
		} else {
			time.Sleep(readyPollSlow)
		}
	}
	if !pageReady(rec) {
		// The slot is kept, so this cycle starts no second watcher.
		triggerReconcile(spec.AppStateFile)
		if !askUntilReady(spec, rec, gone) {
			return
		}
	}
	// The slot is freed first: if the cycle finds the copy not ready after
	// all (one probe of its own failed), it starts a new watcher, which must
	// not find this one still registered.
	readyWatched.Delete(key)
	triggerReconcile(spec.AppStateFile)
}

// askUntilReady asks rec's page until it answers below 500. It reports
// false when the process went away first (gone has cleaned up then). A
// problem is logged once, and again only when it changes.
func askUntilReady(spec manifest.AppSpec, rec state.AppState, gone func() bool) bool {
	said := ""
	say := func(what, line string) {
		if what != said {
			log.Print(line)
			said = what
		}
	}
	retry := currentHooks().PageRetry
	for {
		if gone() {
			return false
		}
		page, ok := askPage(rec, gone, func(after time.Duration) {
			say("slow", fmt.Sprintf("reconcile: %q: copy %d (pid %d) listens on port %d but its page has not answered after %s; still waiting (see shinyctl logs --copy %d)",
				spec.AppName, rec.Copy, rec.PID, rec.Port, after, rec.Copy))
		})
		switch {
		case !ok:
			return false
		case page.Ready:
			markPageReady(rec) // before the cycle reads it
			if said != "" {
				log.Printf("reconcile: %q: copy %d (pid %d): its page answers now", spec.AppName, rec.Copy, rec.PID)
			}
			return true
		case !page.Listening:
			time.Sleep(readyPollSlow) // the port closed meanwhile: dial again
			continue
		}
		if gone() {
			return false // it died while answering: the crash path says why
		}
		setPageProblem(rec, page.Problem)
		say(page.Problem, fmt.Sprintf("reconcile: %q: copy %d (pid %d) listens on port %d but its page %s; it is not ready until the page answers (see shinyctl logs --copy %d)",
			spec.AppName, rec.Copy, rec.PID, rec.Port, page.Problem, rec.Copy))
		time.Sleep(retry)
		retry = min(2*retry, currentHooks().PageRetryMax)
	}
}

// askPage sends one request for rec's page and waits for the answer for as
// long as the process lives. After PageSlow the wait is recorded for status
// and slow is called, once; every readyPollSlow the process is checked, and
// the request cancelled when it is gone. ok is false then.
func askPage(rec state.AppState, gone func() bool, slow func(time.Duration)) (page process.Page, ok bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answer := make(chan process.Page, 1)
	go func() { answer <- currentHooks().PageProbe(ctx, process.InternalHost, rec.Port) }()

	asked, after := time.Now(), currentHooks().PageSlow
	late := time.NewTimer(after)
	defer late.Stop()
	check := time.NewTicker(readyPollSlow)
	defer check.Stop()
	for {
		select {
		case page := <-answer:
			return page, true
		case <-late.C:
			setPageWaiting(rec, asked)
			slow(after)
		case <-check.C:
			if gone() {
				cancel()
				<-answer
				return process.Page{}, false
			}
		}
	}
}

// trustListedCopies marks as answered the page of every running copy that
// the load balancer's saved table lists, and takes its port for its own:
// the daemon before found it ready, and it serves visitors now. It runs
// once, when shinyctl-api starts, before the supervisor, the HTTP server or
// the ticker can run a cycle, so no cycle decides on a serving copy as if it
// were new. The table is copies.json, which the gateway writes after every
// update the load balancer took, and which a load balancer reads when it
// starts; one that is adopted holds that same last update. Only a process
// started strictly before the file was last written can be one it listed: a
// later one on the same port has its page asked. A wall clock stepped
// backwards between a write and a start could trust one process wrongly;
// that is rare, and accepted. A missing file trusts nothing; one that can't
// be read or parsed is logged and trusts nothing.
func trustListedCopies(path string) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var table proxy.Table
	if err == nil {
		var data []byte
		if data, err = os.ReadFile(path); err == nil {
			err = json.Unmarshal(data, &table)
		}
	}
	if err != nil {
		log.Printf("reconcile: %s is not usable (%v); the running copies' pages are asked afresh", path, err)
		return
	}
	written := info.ModTime()
	for _, spec := range appRegistry.All() {
		if !appRegistry.Managed(spec.AppStateFile) {
			continue
		}
		st, err := state.Load(spec.AppStateFile)
		if err != nil {
			continue
		}
		listed := allDials(table[spec.AppStateFile])
		recs := st.Copies()
		for i, up := range process.AliveAll(spec, recs) {
			rec := recs[i]
			if up && rec.Copy >= 1 && rec.StartedAt.Before(written) && slices.Contains(listed, dialOf(rec)) {
				markPageReady(rec)
				ownedPorts.Store(pidPort{rec.PID, rec.Port}, struct{}{})
			}
		}
	}
}

// watchUnreadyCopies starts a ready-watcher on every running copy whose
// page has not answered for this daemon: one that was starting when the
// daemon before stopped, or that the saved table did not list. OnReady calls
// it after refreshPools and syncProxy, so a cycle a watcher triggers never
// sends a table the rebuild has not filled. Like watchRestoredCopies it
// reads the state files without the state lock; a copy that already has a
// watcher keeps it.
func watchUnreadyCopies() {
	for _, spec := range appRegistry.All() {
		if !appRegistry.Managed(spec.AppStateFile) {
			continue
		}
		st, err := state.Load(spec.AppStateFile)
		if err != nil {
			continue
		}
		recs := st.Copies()
		for i, up := range process.AliveAll(spec, recs) {
			if up && recs[i].Copy >= 1 && !pageReady(recs[i]) {
				go watchReady(spec, recs[i])
			}
		}
	}
}
