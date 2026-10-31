# shinyctl

shinyctl keeps R Shiny apps running on one machine, with no root and no
containers: one binary, a YAML file for each app, and `shinyctl apply`. It
restarts an app when it dies, runs it as several copies behind a built-in
load balancer with a warm spare, keeps each visitor on one copy, and
redeploys a new version without cutting off the visitors of the old one. It
is for a lab server, a Raspberry Pi or a small VPS.

The website, with diagrams of how it works, is
[spriyansh.github.io/shinyctl](https://spriyansh.github.io/shinyctl/).

## What you get

- **Restarts.** An app that dies comes back, with a backoff when it keeps
  dying, as `restart-policy` says.
- **Copies and a warm spare.** Each app is one or more R processes ("copies")
  behind a load balancer on the port you chose. A ready spare takes over the
  moment a copy dies.
- **Sticky sessions.** A visitor stays on the copy that holds their Shiny
  session.
- **Redeploys that don't cut visitors off.** New copies start next to the
  old ones; the old ones keep their visitors until those leave.
- **One status view.** `shinyctl status` shows every copy, its role,
  memory, open sessions and crashes, and says what is going on.
- **Start at boot** on Linux, through a systemd user unit.

## Requirements

- Linux or macOS, on amd64 or arm64
- R with the `shiny` package, and `Rscript` on your `PATH`
- systemd, for starting at boot on Linux (optional)
- Go 1.26.10 or newer, only to build it yourself

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/spriyansh/shinyctl/main/install.sh | sh
```

[install.sh](install.sh) picks the build for your system (Linux or macOS, amd64
or arm64), checks it against the release's `checksums.txt`, and copies the one
`shinyctl` binary into `~/.local/bin`. It needs no root, and says if that
folder is not on your `PATH` yet. `SHINYCTL_VERSION=v0.1.0` installs a given
release and `SHINYCTL_INSTALL_DIR` another folder, for example
`curl -fsSL .../install.sh | SHINYCTL_VERSION=v0.1.0 sh`. Run it again to
upgrade.

### By hand

Download the archive for your system and `checksums.txt` from the
[releases page](https://github.com/spriyansh/shinyctl/releases), check the
download, unpack it and put `shinyctl` on your `PATH`. For Linux on arm64
(a Raspberry Pi), version 0.1.0:

```sh
curl -LO https://github.com/spriyansh/shinyctl/releases/download/v0.1.0/shinyctl_0.1.0_linux_arm64.tar.gz
curl -LO https://github.com/spriyansh/shinyctl/releases/download/v0.1.0/checksums.txt
sha256sum --ignore-missing -c checksums.txt        # macOS: shasum -a 256 --ignore-missing -c checksums.txt
tar -xzf shinyctl_0.1.0_linux_arm64.tar.gz
mkdir -p ~/.local/bin && cp shinyctl_0.1.0_linux_arm64/shinyctl ~/.local/bin/
```

The other archives are `linux_amd64`, `darwin_arm64` (Apple silicon) and
`darwin_amd64`. Each unpacks into a folder of its own with `shinyctl`, this
README, the license and notices, and the example in `examples/hello`.

- On Debian and Ubuntu, `~/.local/bin` is on your `PATH` after your next
  login. On macOS, add `export PATH="$HOME/.local/bin:$PATH"` to
  `~/.zprofile`. A file downloaded with a browser on macOS is quarantined;
  `curl` doesn't do that, and `xattr -d com.apple.quarantine shinyctl` lifts
  it.
- With Go 1.26.10 or newer: `go install github.com/spriyansh/shinyctl@latest`.
- From a clone of this repo: `make install`.

Check it with `shinyctl version`. It prints `v0.1.0` for a release (and for
`go install ...@v0.1.0`), and `dev (abc1234)` for a build from a checkout.

## Quick start

```sh
shinyctl init                     # writes ./shinyctl.yaml
$EDITOR shinyctl.yaml             # set entrypoint-file and port
shinyctl apply -f shinyctl.yaml   # starts shinyctl-api, then the app
shinyctl status
```

```
NAME    COPY  ROLE    PORT  PID    STATUS  READY  MEM   SESSIONS  CRASHES  STARTED
my-app  1     active  8080  61429  alive   ready  142M  3         -        2026-10-11T20:20:21+02:00
my-app  2     warm    8080  61430  alive   ready  139M  0         -        2026-10-11T20:20:22+02:00

Memory: 281M in 2 R processes (1 warm), plus 41M for the load balancer.
```

Open `http://127.0.0.1:8080` (the `port` in your manifest).

## Try the example

The example app is in the archive, in `examples/hello`, and in this repo.
From the unpacked archive folder:

```sh
shinyctl apply -f examples/hello/shinyctl.yaml
```

Open http://127.0.0.1:8765 and click **Crash this app**. With the default
warm spare, the page moves to the other copy within about a second: it
reloads by itself and shows that copy's process ID. The crashed copy
restarts a few seconds later (`shinyctl status` shows it `crash-looping`
until then) and becomes the spare, so the roles in `status` have swapped.
`shinyctl logs -f examples/hello/shinyctl.yaml` shows what both copies
printed, including the crash. Remove the app with
`shinyctl delete -f examples/hello/shinyctl.yaml`.

If something else on the machine already uses port 8765, `apply` says so and
`status` shows `port-in-use`. Change `port` in `examples/hello/shinyctl.yaml`
and apply again.

## How it works

- **`shinyctl-api` and the load balancer.** `shinyctl-api` is a per-user
  background process that `apply` starts when it is not running. It starts
  your apps, restarts them, and tells the load balancer where they are. The
  load balancer, a process of its own, listens on each app's port and sends
  every request to one of the app's copies. Apps and the load balancer keep
  running if `shinyctl-api` dies; a new `shinyctl-api` adopts them.
- **Copies and roles.** Of the copies that are running and ready, the oldest
  `replicas` are *active* and take new visitors. The rest are *warm*:
  running, ready, and given no new visitors while every active copy is up
  (a visitor whose cookie names a warm copy stays on it). A copy that is
  being replaced is *draining* (see Redeploys). `status` shows each copy's
  role.
- **Takeover.** When an active copy fails, the load balancer sends its
  visitors to a warm copy at once, and `shinyctl-api` makes that copy active
  within milliseconds; the visitors stay on it. This holds for copies that
  an earlier `shinyctl-api` started too. A page with the example's script
  reloads by itself within about a second and lands on the warm copy. The
  dead copy restarts after its backoff and comes back as the newest copy, so
  it is the spare: roles don't swap back on their own. While `shinyctl-api`
  is down the load balancer still puts the spare in, but nothing restarts the
  dead copy until `shinyctl-api` is up again. Where the system can't watch
  copies from an earlier run (Linux before 5.3, or a sandbox that forbids
  it), `shinyctl-api` notices them at its next check, every 5 seconds; the
  load balancer still puts the spare in.
- **Sticky sessions.** A visitor stays on one copy, for as long as it is
  running, through a cookie named `lb_<port>` (for example `lb_8080`) that
  the load balancer sets. Copies don't share memory, so a Shiny session
  has to stay where its R process is. Stickiness needs the browser to
  keep that cookie: it doesn't hold for an app embedded in another site's
  frame, or in Safari with third-party cookies blocked.
- **`lb-policy`** picks the copy for a visitor who has none, among the
  active copies: `least-conn` (the fewest open connections, the default),
  `round-robin`, `random`, or `first` (always the oldest active copy).
- **Redeploys.** `restart`, and an `apply` that changes `env`,
  `working-dir`, `app-group`, `app-name` or `entrypoint-file`, start new
  copies next to the running ones and return at once: `replicas` of them
  first, the spares once those have taken over.
  - Once the new copies that serve are ready, new visitors go to them and
    the old copies *drain*: they get no new visitors and keep the ones they
    have until those sessions end or `drain-timeout` (15 minutes by default)
    passes. An old spare with no visitors stops right away.
  - A page left open on an old copy keeps working on the old version, and
    reloading it brings the new one.
  - When an old copy stops at the timeout, a page with the example's script
    reloads by itself onto the new version; other pages grey out until
    reloaded.
  - An old copy with no sessions stops within about 15 seconds.
  - For the few seconds the new spare takes to start the app has no spare.
  - A new version that fails at start never takes over: the old copies
    keep serving, and `status` says which copy doesn't start and where to
    look.
  - `--now` stops the old copies as soon as the new ones are ready.
  - A redeploy during another goes straight to the newest version. At most
    `replicas + warm-replicas` copies with visitors drain at once; beyond
    that, the ones that have drained longest stop.

  While it goes on, `status` shows the draining copies and how long they
  have left:

  ```
  NAME   COPY  ROLE      PORT  PID    STATUS  READY  MEM   SESSIONS  CRASHES  STARTED
  hello  1     draining  8765  61429  alive   ready  142M  2         -        2026-10-27T10:00:01+02:00
  hello  3     active    8765  61501  alive   ready  140M  1         -        2026-10-27T11:30:05+02:00
  hello  4     warm      8765  61502  alive   ready  138M  0         -        2026-10-27T11:30:05+02:00

  Memory: 420M in 3 R processes (1 warm, 1 draining), plus 41M for the load balancer.

  hello: copy 1 is draining with 2 sessions; it stops when they end, or in 15m at the latest.
  ```
- **Scaling.** Changing `replicas` or `warm-replicas` starts copies, or drains
  the ones it removes the same way: they get no new visitors, and stop when
  their sessions end or at `drain-timeout`. Copies that are starting or dead
  stop at once, and `--now` stops them all at once. Copies are retired in
  this order: with no process, still starting, then ready ones with the
  fewest open sessions (a warm copy before an active one when they tie).
- **Restarts and crashes.** `restart-policy: always` restarts after any
  exit, `on-failure` only after a non-zero exit, `never` leaves the app
  down. A crash is noticed the moment it happens, and a check every 5
  seconds catches anything else. Crashes are counted and backed off for each
  copy: 2 seconds, doubling up to 1 minute. While an app restarts, a request
  to its port waits up to 15 seconds for it; after that the visitor gets a
  page saying the app is restarting, which reloads itself every few seconds.
  After a restart, an open page greys out until it is reloaded. The example
  reloads it by itself once the app answers again: copy its `tags$script`
  into your app's UI. Shiny's own `session$allowReconnect("force")` is no
  substitute: with shiny 1.14, a page reconnected to a new R process keeps
  its old outputs, and its downloads stop working.
- **Memory.** Each copy is a full R process: with the defaults every app
  runs two. A small app takes about 110-140 MB per copy on a Raspberry Pi 5.
  During a redeploy both versions run side by side: the peak is the old
  copies plus `replicas` new ones (3 processes with the defaults, for the
  few seconds R takes to start), and old copies that still have visitors run
  until `drain-timeout`. The peak can be one higher, for up to about 15
  seconds, if the load balancer is restarting at the moment of the switch, or
  if a second restart comes within about 15 seconds of the previous one's
  switch. On a small machine, set `warm-replicas: 0`, a shorter
  `drain-timeout`, or use `--now`. `status` shows the total.

## Manifest

```yaml
app-group: lab                  # required
app-name: explorer              # required
entrypoint-file: ./app.R        # required
port: 8080                      # required; not 20000-29999
app-state-file: ./state.json    # required; identifies the app to stop and delete
host: 0.0.0.0                   # default 0.0.0.0 (all interfaces); 127.0.0.1 = this machine only
restart-policy: always          # always (default), on-failure, or never
working-dir: ./                 # default: the entrypoint-file's directory
env:                            # added on top of shinyctl-api's own environment
  R_LIBS_USER: /home/me/Rlibs
replicas: 2                     # copies that serve visitors; default 1; 1-16
warm-replicas: 1                # ready spares that take over at once; default 1; 0-16; 0 turns them off
lb-policy: least-conn           # how a new visitor picks a copy: least-conn (default), round-robin, random, first
drain-timeout: 15m              # how long replaced copies keep their visitors; default 15m; 0 to 24h; 0 stops them at once
```

| Field | Required / default | Meaning and limits |
|---|---|---|
| `app-group` | required | Names the app together with `app-name`. No `/` or `@`: they name the log files. |
| `app-name` | required | The app's name in `status` and in the log file names. No `/` or `@`. |
| `entrypoint-file` | required | The R file that starts the app. A running process is recognised by this path in its command line. |
| `port` | required | The port the app is served on. Not between 20000 and 29999. |
| `app-state-file` | required | Where shinyctl keeps the app's state. It identifies the app: changing it makes a different app. |
| `host` | `0.0.0.0` | An IP address, not a hostname. `0.0.0.0` listens on IPv4 only and `::` on IPv6 only. `127.0.0.1` is this machine only. |
| `restart-policy` | `always` | `always`, `on-failure` or `never`. |
| `working-dir` | the entrypoint's folder | The folder the app starts in. |
| `env` | none | Variables added on top of `shinyctl-api`'s environment. |
| `replicas` | 1 | Copies that serve visitors, 1 to 16. |
| `warm-replicas` | 1 | Ready spares, 0 to 16. Each is an R process. |
| `lb-policy` | `least-conn` | `least-conn`, `round-robin`, `random` or `first`. |
| `drain-timeout` | `15m` | A duration such as `90s`, `15m` or `1h`, from `0` to `24h`; a number alone is refused. `0` stops replaced copies at once. |

- **Relative paths resolve from the manifest's folder**, so commands work
  from any directory. `~` is not expanded.
- **`port` can't be between 20000 and 29999.** shinyctl runs each app's R
  process on a port in that range, on 127.0.0.1, behind the load balancer
  that listens on `port`. Your app sees requests coming from 127.0.0.1; the
  visitor's address is the first entry of its `X-Forwarded-For` header. When
  a web server on the same machine in front of the port (nginx terminating
  TLS, say) sends its own `X-Forwarded-For`, `X-Forwarded-Proto` and
  `X-Forwarded-Host`, they reach the app, with 127.0.0.1 added to the end of
  `X-Forwarded-For`. Those headers from anywhere else are replaced. Each
  copy gets a port of its own in that range, and `shinyctl-api` checks that
  the socket on it belongs to the copy before it sends visitors there: if
  another program holds the port, the copy is moved to another one.
- **`shinyctl-api` inherits the environment of the shell that started
  it.** Apps get that environment plus `env`.

## Commands

| Command | What it does |
|---|---|
| `init` | Writes a starter manifest. `-o` sets the path, `--force` overwrites. |
| `apply -f <file>` | Starts the app, or updates it if the manifest changed (see below). Starts `shinyctl-api` on first use. Also resumes a stopped app. `--now` stops copies that are replaced or no longer wanted without waiting for their visitors. |
| `status` | Lists every app `shinyctl-api` manages. Never starts `shinyctl-api`. |
| `stop -f <file>` | Sends SIGTERM to each of the app's processes, waits 10 seconds, then SIGKILL, and closes the app's port. The app stays down until the next `apply` or `restart`. |
| `restart -f <file>` | Starts new copies from the manifest as it is now, so new code in `app.R` and other app files takes effect, and returns at once. Once they are ready they take new visitors; the old copies keep theirs until those leave or `drain-timeout` passes (see Redeploys). `--now` stops the old copies as soon as the new ones are ready. Also starts a stopped or crash-looping app right away. |
| `logs -f <file>` | Prints the last 100 lines of the app's log. `--copy N` shows one copy; without it every copy is shown. `--follow` keeps printing until Ctrl-C, `-n` sets the number of lines, `--api` shows `shinyctl-api`'s log instead. Never starts `shinyctl-api`. |
| `delete -f <file>` | Stops the app, forgets it and removes its state file. Logs are kept. |
| `startup` | Prints a systemd user unit that starts `shinyctl-api` at boot (Linux), and on stderr how to install it. |
| `serve` | Runs `shinyctl-api` in the foreground. `-d` runs it in the background. |
| `version` | Prints the version: `v0.1.0` for a release or `go install ...@v0.1.0`, `dev (abc1234)` for a build from a checkout (`, modified` when it has uncommitted changes). |

`apply` replaces the copies of a running app, as `restart` does, when a
manifest field that shapes its process changed: `app-group`, `app-name`,
`entrypoint-file`, `working-dir` or `env`. A changed `port`, `host`,
`replicas`, `warm-replicas`, `lb-policy`, `drain-timeout` or
`restart-policy` takes effect without restarting the app's processes
(copies are started, or drained, as needed). `apply` never looks at `app.R`
or other app files: run `restart` for code changes.

`stop`, `restart`, `delete` and a re-apply signal a process only if its
command line contains the app's `entrypoint-file`. If the recorded process
ID now belongs to something else, shinyctl leaves it alone, and `stop`,
`restart` or `delete` names it in the reply.

## Reading `status`

The columns:

- `COPY`, `ROLE` (`active`, `warm` or `draining`) and `PORT`, the app's
  public port.
- `PID` and `STARTED`: the copy's R process and when it started.
- `STATUS` is `alive`, `dead`, `stopped`, `crash-looping`, `exited` or
  `port-in-use`:
  - `crash-looping` is a copy that keeps dying and is being restarted with a
    backoff; `dead` is a copy that is not running and not counted as a
    crash.
  - `exited` is a copy that stopped and that `restart-policy` leaves down on
    purpose (`never`, or a clean exit under `on-failure`); a line under the
    table says so, and `apply` or `restart` starts it again.
  - `port-in-use` means another program holds the app's port, so no copy was
    started; the reason is printed under the table, nothing is counted as a
    crash, and the next check tries again.
- `READY` turns from `starting` to `ready` once the copy's R process accepts
  connections and the load balancer lists it.
- `MEM` is the memory the copy's main R process takes, counting what the
  system has compressed or swapped out: on macOS the number Activity Monitor
  shows, on Linux resident plus swapped memory (`VmRSS` + `VmSwap`). On an
  idle Mac it can be lower than `ps` shows, and under memory pressure much
  higher. Worker processes the copy started aren't counted. When the number
  can't be read, `MEM` falls back to resident memory from `ps`, or shows `-`,
  and `api.log` says why.
- `SESSIONS` is the number of requests open to the copy, a Shiny session
  being one open WebSocket.
- `CRASHES` counts crashes in a row: it goes back to `-` once the copy has
  stayed up for a minute.

Under the table, `status` says what the table can't:

- a `Memory:` line with the total of the running copies, plus the load
  balancer, which isn't counted in any copy (about 40 MB on Linux, 12 MB on a
  Mac);
- an app's port that is not served, and why;
- an app that was not started, and why;
- a redeploy that has not taken over yet: which copy is starting, and, if the
  new version does not start, which copy and where to look;
- draining copies: how many sessions are left and when `drain-timeout` ends
  them at the latest;
- copies that `exited`, and why;
- an app without a spare for the seconds between a switch and its new spare
  being ready.

## HTTPS and the internet

shinyctl serves plain HTTP. To put an app on the internet over HTTPS, set
`host: 127.0.0.1` in its manifest, so only this machine can reach the port,
and put one of these in front.

**nginx**, with a certificate from Let's Encrypt (for example through
[certbot](https://certbot.eff.org/)). Shiny needs the WebSocket upgrade and a
long read timeout:

```nginx
map $http_upgrade $connection_upgrade { default upgrade; '' close; }
server {
    listen 443 ssl;
    server_name shiny.example.org;
    ssl_certificate     /etc/letsencrypt/live/shiny.example.org/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/shiny.example.org/privkey.pem;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;
        proxy_read_timeout 1d;
        proxy_buffering off;
    }
}
```

To serve the app under a path, use `location /myapp/ { proxy_pass http://127.0.0.1:8080/; ... }`
with both slashes, so the app sees `/`, and send the address without the
final slash to the one with it:

```nginx
location = /myapp { return 301 /myapp/; }
```

**Cloudflare Tunnel** needs no open port. For a quick try:

```sh
cloudflared tunnel --url http://127.0.0.1:8080
```

For a permanent address, create a named tunnel (see Cloudflare's
[documentation](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)).
WebSockets work as they are.

**Tailscale** serves the app inside your tailnet over HTTPS, or to the
internet with Funnel (see [Serve](https://tailscale.com/kb/1312/serve) and
[Funnel](https://tailscale.com/kb/1223/funnel)):

```sh
tailscale serve --bg 8080       # HTTPS inside your tailnet
tailscale funnel --bg 8080      # public
```

What shinyctl does with these: a web server on the same machine in front of
the port is trusted for `X-Forwarded-*`, so the app sees the visitor's
address and that the request was HTTPS. The cookie that keeps a visitor on a
copy becomes `Secure; SameSite=None` when that server says the request was
`https`.

## Start at boot (Linux)

Nothing restarts `shinyctl-api` after a reboot by itself. `shinyctl startup`
prints a systemd user unit for it, and on stderr the commands to install it:

```sh
mkdir -p ~/.config/systemd/user && mkdir -p -m 700 ~/.shinyctl
shinyctl startup > ~/.config/systemd/user/shinyctl.service
systemctl --user daemon-reload
pkill -u "$USER" -f '(^|/)shinyctl serve$'
systemctl --user enable --now shinyctl
loginctl enable-linger "$USER"
```

- Stop a `shinyctl-api` you started by hand first (the `pkill` line). It
  would hold the socket and make the service's copy exit. Its apps and the
  load balancer keep running, and the service's `shinyctl-api` adopts
  them.
- The unit uses the `PATH` of the shell you ran `startup` in, plus
  `Rscript`'s directory. Under systemd, apps get the user manager's
  environment, that `PATH`, and the manifest's `env`.
- Run `startup` again after moving `shinyctl` or R.
- On macOS, run `shinyctl serve -d` after a reboot.

## Upgrading shinyctl

Replace the binary as in Install, then restart `shinyctl-api`:

```sh
systemctl --user restart shinyctl                 # if you installed the service
# or, if you started it by hand:
pkill -u "$USER" -f '(^|/)shinyctl serve$' && shinyctl serve -d
```

Your apps keep running: the new `shinyctl-api` adopts them. If the new
version comes with a new load balancer, it is started next to the old one
and takes over without refusing a connection. The old one answers the
requests it is in the middle of, for up to five seconds, and then stops.
Connections that were open on it (a Shiny session is one) end, so a page open
at that moment reconnects or reloads once (pages with the example's script do
it by themselves). On Linux, a request that reaches the old one in the very
instant it closes can fail once.

## Stopping and uninstalling

`shinyctl-api` and the load balancer keep running after you remove the
`shinyctl` binary, and the load balancer keeps every app's port open. To stop
shinyctl completely, for example before uninstalling or downgrading it,
delete the apps, stop the service if you installed one, then stop the two
processes:

```sh
shinyctl delete -f shinyctl.yaml                       # for each app
systemctl --user disable --now shinyctl                # Linux, if you installed the service
pkill -u "$USER" -f '(^|/)shinyctl (serve|proxy --config .*)$'
```

The pattern matches only `shinyctl serve` and `shinyctl proxy --config …` of
your user, not other `shinyctl` commands. An app you didn't delete keeps
running, and its port stays closed once the load balancer is gone.

To remove shinyctl itself:

```sh
rm -f ~/.config/systemd/user/shinyctl.service && systemctl --user daemon-reload   # Linux, if you installed the service
rm -f ~/.local/bin/shinyctl                       # or wherever you put it
rm -rf ~/.shinyctl                                # logs, settings and the load balancer's files
```

## Files

Everything lives under `~/.shinyctl/`:

| Path | Contents |
|---|---|
| `api.log` | `shinyctl-api`'s own log, and the load balancer's |
| `logs/<app-group>-<app-name>.log` | Copy 1's stdout and stderr; copy N's are in `…@N.log`. A line marks each start. |
| `registry.json` | Every app `shinyctl-api` manages |
| `api.sock` | The socket the CLI talks to |
| `proxy/` | The load balancer's settings, copies, cookie key, lock and socket |

Logs rotate at 1 GiB, keeping one previous copy as `.log.1`.

## Limitations

- **A changed port that's in use fails the apply**, and the app stays on
  its old port. Changing `app-state-file` makes it a different app, so
  `delete` the old one with its old manifest first.
- **Apps keep running if `shinyctl-api` dies, but nothing watches them.**
  Run `shinyctl serve -d` or any `apply` to bring it back. It resumes every
  app it was managing. On Linux, [the unit above](#start-at-boot-linux)
  starts it at boot and restarts it whenever it dies. The load balancer
  keeps serving them, but if it dies too, every app is unreachable until
  `shinyctl-api` is back.
- **Logging out can kill everything on systemd servers.** Enable
  lingering with `loginctl enable-linger $USER` if your server allows it.
  `shinyctl-api` writes a warning to `api.log` when lingering is off.
- **`stop` signals only the main R process.** Worker processes the app
  started, for example through callr or future, can outlive it.
- **A drain ends at `drain-timeout`.** Sessions still open then are cut;
  pages with the example's script reload onto the new version.
- **A tab left open is a session**, and so is a visitor whose connection
  vanished without closing (a laptop lid closed): the load balancer notices
  that within about 2.5 minutes, or, for an app that keeps sending to the
  page, only at `drain-timeout`.
- **Two tabs of one app in one browser during a redeploy:** reloading one
  moves that browser to the new version; the other tab keeps its session,
  but its downloads and uploads fail until it is reloaded too.
- **Going back to the page during a redeploy** shows the old copy's page from
  the browser's cache first; the example's script then reloads it onto the new
  version. Without that script the page reconnects to the old copy.
- **Both versions run side by side** for up to `drain-timeout`, so a change
  to shared data (a database schema, files) must suit both.
- **A new version that starts but fails on its page still takes over:** a
  copy counts as ready once R accepts connections.
- **Copies don't share memory.** Anything a session keeps in R lives in
  its copy.
- **The load balancer is in the path of every request.** If it dies,
  `shinyctl-api` restarts it within about a second, and every app is
  unreachable until then.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development and releases.

## License

MIT. See [LICENSE](LICENSE). The release archives also carry
`THIRD_PARTY_NOTICES`, with the licenses of the code shinyctl includes.
