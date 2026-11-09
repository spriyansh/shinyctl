package process

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// lsofTimeout bounds the lookup: it runs once per copy, while the copy starts.
const lsofTimeout = 5 * time.Second

// listenersRun runs a command and returns its output; a variable so that the
// tests can stand in for lsof, which as an ordinary user sees only this
// user's processes. The copy's tree comes from the process table (readTable),
// which the tests stand in for too.
var listenersRun = run

// portOwner decides from what is always visible, the copy's own process tree:
// lsof lists every socket of this user's processes and none of another
// user's, so "who else listens" can't be asked of it (another user's listener
// would read as "nobody"). So: connect first; a port that doesn't accept has
// no owner yet. A port that accepts and has no listening socket in the copy's
// tree is another program's. The connect comes before the lookup, so a copy
// that binds in between is found and is not taken for a stranger.
func portOwner(pid, port int) Owner {
	if !accepts(port) {
		return OwnerNone
	}
	table, err := readTable()
	if err != nil {
		return OwnerUnknown
	}
	var pids []string
	for _, p := range descendants(pid, parents(table)) {
		pids = append(pids, strconv.Itoa(p))
	}
	mine, ok := lsofListeners(port, strings.Join(pids, ","))
	if !ok {
		return OwnerUnknown
	}
	if len(mine) > 0 {
		return OwnerSelf
	}
	return OwnerOther
}

// accepts says the port takes a TCP connection on the internal address.
func accepts(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(InternalHost, strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// lsofListeners returns the processes among pids (a comma list) that listen on
// the TCP port. lsof exits 1 when it finds nothing; -b and -w keep it from
// blocking on a stale mount and from printing warnings.
func lsofListeners(port int, pids string) ([]int, bool) {
	args := []string{"-a", "-p", pids, "-b", "-w", "-n", "-P", "-iTCP:" + strconv.Itoa(port), "-sTCP:LISTEN", "-Fp"}
	out, err := listenersRun("/usr/sbin/lsof", args...)
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return nil, false
		}
	}
	var found []int
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "p") {
			if n, err := strconv.Atoi(line[1:]); err == nil {
				found = append(found, n)
			}
		}
	}
	return found, true
}

func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lsofTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	out, err := cmd.Output()
	return string(out), err
}
