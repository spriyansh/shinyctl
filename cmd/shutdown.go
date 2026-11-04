/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/process"
	"github.com/spriyansh/shinyctl/internal/proxy"
	"github.com/spriyansh/shinyctl/internal/registry"
	"github.com/spriyansh/shinyctl/internal/state"
)

var shutdownYes bool

var shutdownCmd = &cobra.Command{
	Use:   "shutdown",
	Short: "Stop every app, the load balancer and shinyctl-api",
	Args:  cobra.NoArgs,

	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := newShutdowner(shutdownYes, cmd.OutOrStdout(), cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		return s.run()
	},
}

func init() {
	shutdownCmd.Flags().BoolVarP(&shutdownYes, "yes", "y", false, "stop without asking")
	rootCmd.AddCommand(shutdownCmd)
}

// runningApp is an app that has a live copy, and how many sessions are open
// to it when that is known.
type runningApp struct {
	name     string
	sessions *int
}

// shutdowner is the shutdown command with everything it touches as a field,
// so the order of what it does can be tested without processes.
type shutdowner struct {
	yes         bool
	linux       bool                                            // adds the boot clause to the prompt
	isTerminal  func() bool                                     // whether there is someone to ask
	in          io.Reader                                       // the answer
	dialable    func() bool                                     // whether shinyctl-api answers on its socket
	status      func() (api.StatusResponse, error)              // shinyctl-api's /status
	leftovers   func() (balancer int, apps []string, err error) // what is running without shinyctl-api, from disk
	service     func() (stopped bool, err error)                // stops the shinyctl systemd service when it is active
	start       func() error                                    // starts shinyctl-api
	shutdown    func() (api.ShutdownResponse, error)            // asks shinyctl-api to stop everything and exit
	wait        time.Duration                                   // how long the socket may go on answering after shutdown
	logPath     string                                          // api.log, for the message when it does
	out, errOut io.Writer
}

func newShutdowner(yes bool, out, errOut io.Writer) (*shutdowner, error) {
	socketPath, err := process.SocketPath()
	if err != nil {
		return nil, err
	}
	logPath, err := process.LogPath()
	if err != nil {
		return nil, err
	}
	proxyDir, err := process.ProxyDir()
	if err != nil {
		return nil, err
	}
	registryPath, err := process.RegistryPath()
	if err != nil {
		return nil, err
	}

	return &shutdowner{
		yes:        yes,
		linux:      runtime.GOOS == "linux",
		isTerminal: func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
		in:         os.Stdin,
		dialable:   func() bool { return process.SocketDialable(socketPath) },
		status:     func() (api.StatusResponse, error) { return api.Status(socketPath) },
		leftovers:  func() (int, []string, error) { return leftovers(proxyDir, registryPath) },
		service:    func() (bool, error) { return stopService(realServiceEnv()) },
		start:      process.EnsureAPIRunning,
		shutdown:   func() (api.ShutdownResponse, error) { return api.Shutdown(socketPath) },
		wait:       10 * time.Second,
		logPath:    logPath,
		out:        out,
		errOut:     errOut,
	}, nil
}

// leftovers is what is running while shinyctl-api is not: the load balancer
// holding its lock, and the apps (by name) with a live copy. It only reads.
func leftovers(proxyDir, registryPath string) (balancer int, apps []string, err error) {
	balancer = proxy.RunningPID(proxy.SettingsFor(proxyDir).ConfigPath)

	reg, err := registry.Open(registryPath)
	if err != nil {
		return 0, nil, err
	}
	for _, spec := range reg.All() {
		st, err := state.Load(spec.AppStateFile)
		if err != nil {
			return 0, nil, err
		}
		for _, alive := range process.AliveAll(spec, st.Copies()) {
			if alive {
				apps = append(apps, spec.AppName)
				break
			}
		}
	}
	sort.Strings(apps)
	return balancer, apps, nil
}

// pollEvery is how often shutdown looks for the socket to be gone.
const pollEvery = 50 * time.Millisecond

func (s *shutdowner) run() error {
	// What is running comes first and changes nothing: the answer to the
	// question is needed before anything is stopped.
	up := s.dialable()
	running, err := s.runningApps(up)
	if err != nil {
		return err
	}

	if len(running) > 0 && !s.yes {
		if !s.isTerminal() {
			return fmt.Errorf("shinyctl shutdown would stop running apps (%s) and has no terminal to ask; run shinyctl shutdown --yes to stop them without asking",
				listNames(namesOf(running)))
		}
		fmt.Fprint(s.errOut, s.prompt(running))
		answer, _ := bufio.NewReader(s.in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			fmt.Fprintln(s.out, "nothing was stopped")
			return nil
		}
	}

	serviceStopped, err := s.service()
	if err != nil {
		return fmt.Errorf("stopping the shinyctl service: %w", err)
	}
	if serviceStopped {
		fmt.Fprintln(s.out, "stopped the shinyctl service (still enabled: systemctl --user disable shinyctl keeps it from starting at boot)")
	}

	// No shinyctl-api: it crashed, or the service has just been stopped. What
	// it left behind is stopped by a daemon of the same build, which adopts
	// it and stops it as it always does.
	if !s.dialable() {
		balancer, apps, err := s.leftovers()
		if err != nil {
			return err
		}
		if balancer == 0 && len(apps) == 0 {
			if !serviceStopped {
				fmt.Fprintln(s.out, "nothing is running")
			}
			return nil
		}
		if !serviceStopped {
			fmt.Fprintln(s.out, "shinyctl-api is not running; starting it to stop what it left behind")
		}
		if err := s.start(); err != nil {
			return err
		}
	}

	fmt.Fprintln(s.errOut, "stopping; an app busy in R can take 10 seconds to stop")
	resp, err := s.shutdown()
	if errors.Is(err, api.ErrNoShutdown) {
		return errors.New(`shinyctl-api is an older build without shutdown; stop it with pkill -u "$USER" -f '(^|/)shinyctl serve$' and run shinyctl shutdown again`)
	}
	if err != nil {
		return err
	}

	s.report(resp)

	// Gone means the socket no longer answers. Its pid is not waited on: a
	// daemon this command started is its child, and a child that has exited
	// is a zombie that still looks alive until the command does.
	deadline := time.Now().Add(s.wait)
	for s.dialable() {
		if time.Now().After(deadline) {
			return fmt.Errorf("shinyctl-api (pid %d) still answers after %s; see %s", resp.PID, s.wait, s.logPath)
		}
		time.Sleep(pollEvery)
	}
	// A daemon started just for this is an implementation detail, and so is
	// the service's: only the one the user was running gets a line.
	if up && !serviceStopped {
		fmt.Fprintf(s.out, "stopped shinyctl-api (pid %d)\n", resp.PID)
	}

	if len(resp.Problems) > 0 {
		for _, p := range resp.Problems {
			fmt.Fprintf(s.errOut, "warning: %s\n", p)
		}
		return errors.New("some of it could not be stopped; see the warnings above")
	}
	return nil
}

// runningApps lists the apps with a live copy: from shinyctl-api when it
// answers, from the state files when it does not.
func (s *shutdowner) runningApps(up bool) ([]runningApp, error) {
	var running []runningApp
	if !up {
		_, names, err := s.leftovers()
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			running = append(running, runningApp{name: n})
		}
		return running, nil
	}

	resp, err := s.status()
	if err != nil {
		return nil, err
	}
	for _, app := range resp.Apps {
		if app.Stopped {
			continue
		}
		var alive bool
		var sessions *int
		for _, c := range app.Copies {
			if !c.Alive {
				continue
			}
			alive = true
			if c.Sessions != nil {
				if sessions == nil {
					sessions = new(int)
				}
				*sessions += *c.Sessions
			}
		}
		if alive {
			running = append(running, runningApp{name: app.Name, sessions: sessions})
		}
	}
	return running, nil
}

// prompt is the question, with the running apps listed.
func (s *shutdowner) prompt(running []runningApp) string {
	var b strings.Builder
	b.WriteString("shinyctl shutdown stops every app, the load balancer and shinyctl-api. Running now:\n")
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, app := range running {
		switch {
		case app.sessions == nil:
			fmt.Fprintf(w, "  %s\n", app.name)
		case *app.sessions == 0:
			fmt.Fprintf(w, "  %s\tno open sessions\n", app.name)
		case *app.sessions == 1:
			fmt.Fprintf(w, "  %s\t1 open session\n", app.name)
		default:
			fmt.Fprintf(w, "  %s\t%d open sessions\n", app.name, *app.sessions)
		}
	}
	w.Flush()
	if s.linux {
		b.WriteString("They start again with shinyctl serve -d, any apply, or at boot if the shinyctl service is enabled.\n")
	} else {
		b.WriteString("They start again with shinyctl serve -d or any apply.\n")
	}
	b.WriteString("Stop everything? [y/N] ")
	return b.String()
}

// report says what was stopped.
func (s *shutdowner) report(resp api.ShutdownResponse) {
	switch len(resp.Apps) {
	case 0:
		fmt.Fprintln(s.out, "no app was running")
	case 1:
		fmt.Fprintf(s.out, "stopped the app %s\n", listNames(resp.Apps))
	default:
		fmt.Fprintf(s.out, "stopped the apps %s\n", listNames(resp.Apps))
	}
	for _, note := range resp.Notes {
		fmt.Fprintln(s.out, note)
	}
	if resp.Balancer {
		fmt.Fprintln(s.out, "stopped the load balancer")
	}
}

func namesOf(apps []runningApp) []string {
	names := make([]string, len(apps))
	for i, a := range apps {
		names[i] = a.name
	}
	return names
}

// listNames quotes names and joins them for a sentence: "a", "a" and "b",
// "a", "b" and "c".
func listNames(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	if len(quoted) < 2 {
		return strings.Join(quoted, "")
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
}
