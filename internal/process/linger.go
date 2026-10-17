package process

import (
	"fmt"
	"os/exec"
	"os/user"
	"strings"
)

// LingerWarning returns a human-readable warning if this process risks being
// killed when the current user logs out, or "" if there's nothing to warn
// about — either because systemd lingering is already enabled, or because
// this isn't a systemd-managed Linux machine where the concern even applies
// (e.g. macOS, which has no loginctl at all).
func LingerWarning() string {
	if _, err := exec.LookPath("loginctl"); err != nil {
		return ""
	}

	u, err := user.Current()
	if err != nil {
		return ""
	}

	out, err := exec.Command("loginctl", "show-user", u.Username, "--property=Linger").Output()
	if err != nil {
		return ""
	}

	if strings.TrimSpace(string(out)) != "Linger=no" {
		return ""
	}

	return fmt.Sprintf(
		"warning: lingering is not enabled for %q — this process will be killed when you log out.\n"+
			"Run `loginctl enable-linger %s` to keep it running, or ask your sysadmin to. Or simply run `shinyctl serve` in a terminal that you don't close, e.g. inside a tmux session.\n",
		u.Username, u.Username,
	)
}
