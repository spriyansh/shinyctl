/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/logtail"
	"github.com/spriyansh/shinyctl/internal/process"
)

// detachWait is how long `serve -d` waits for the new shinyctl-api to answer.
var detachWait = 5 * time.Second

// detachPoll is how often it looks.
const detachPoll = 50 * time.Millisecond

// detacher is `serve -d`: it starts shinyctl-api in the background and says
// "started" only once it answers (its /healthz does, after the registry is
// open and the load balancer is being set up; the socket is bound much
// earlier). The parts are fields so that the three outcomes can be tested
// with a fake child.
type detacher struct {
	socketPath     string
	logPath        string
	alreadyRunning func() bool
	healthy        func() (pid int, ok bool) // who answers /healthz, if anyone
	start          func() (pid int, exited <-chan struct{}, err error)
	wait           time.Duration
	out            io.Writer
}

// newDetacher is the real `serve -d`: it starts exePath as `serve` in the
// background and asks the socket whether shinyctl-api is up.
func newDetacher(exePath, socketPath, logPath string) detacher {
	return detacher{
		socketPath:     socketPath,
		logPath:        logPath,
		alreadyRunning: func() bool { return process.SocketDialable(socketPath) },
		healthy:        func() (int, bool) { return api.Healthy(socketPath) },
		start: func() (int, <-chan struct{}, error) {
			child, err := process.StartDetached(exePath, []string{"serve"}, logPath)
			if err != nil {
				return 0, nil, err
			}
			exited := make(chan struct{})
			go func() {
				child.Wait()
				close(exited)
			}()
			return child.Process.Pid, exited, nil
		},
		wait: detachWait,
		out:  os.Stdout,
	}
}

func (d detacher) run() error {
	if d.alreadyRunning() {
		fmt.Fprintf(d.out, "shinyctl-api is already running (socket %s)\n", d.socketPath)
		return nil
	}

	pid, exited, err := d.start()
	if err != nil {
		return err
	}

	deadline := time.Now().Add(d.wait)
	for {
		answered, ok := d.healthy()
		if ok {
			// Whoever answers is not necessarily the child: two started at
			// once, one loses the socket and exits, and the other answers. A
			// daemon that names itself settles it; one that doesn't is taken
			// for the child unless the child has gone.
			gone := false
			select {
			case <-exited:
				gone = true
			default:
			}
			if (answered != 0 && answered != pid) || (answered == 0 && gone) {
				if answered != 0 {
					fmt.Fprintf(d.out, "shinyctl-api is already running (pid %d, socket %s)\n", answered, d.socketPath)
				} else {
					fmt.Fprintf(d.out, "shinyctl-api is already running (socket %s)\n", d.socketPath)
				}
				return nil
			}
			break
		}
		select {
		case <-exited:
			return fmt.Errorf("shinyctl-api exited right after starting; see %s%s", d.logPath, lastLogLine(d.logPath))
		case <-time.After(detachPoll):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("shinyctl-api (pid %d) did not answer within %s; see %s%s", pid, d.wait, d.logPath, lastLogLine(d.logPath))
		}
	}

	fmt.Fprintf(d.out, "shinyctl-api started in background: pid %d, logs at %s\n", pid, d.logPath)
	return nil
}

// lastLogLine is ": <the last line of the log>" for an error message, or
// nothing when the log has none. Only the end of the file is read: the log
// can be a gigabyte.
func lastLogLine(path string) string {
	var buf bytes.Buffer
	// A few lines, in case the last ones are blank.
	if _, err := logtail.Last(path, 5, &buf); err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return ": " + last
	}
	return ""
}
