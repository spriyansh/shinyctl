package process

import (
	"context"
	"fmt"
	"os/exec"
	"os/user"
	"strings"
	"time"
)

// lingerTimeout is how long LingerWarning waits for loginctl.
const lingerTimeout = 2 * time.Second

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

	// A slow logind must not hold up the start of shinyctl-api.
	ctx, cancel := context.WithTimeout(context.Background(), lingerTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "loginctl", "show-user", u.Username, "--property=Linger")
	cmd.WaitDelay = time.Second // and not for a child of it that holds the pipe
	out, err := cmd.Output()
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
