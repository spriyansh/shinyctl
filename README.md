# shinyctl

Keeps R Shiny apps running on a single machine. No root, no containers.

You describe an app in a YAML manifest and run `shinyctl apply`. A per-user
background process, `shinyctl-api`, starts the app and restarts it whenever
it dies. Each app runs as one R process behind a built-in load balancer,
which listens on the port you chose. While the app restarts, requests to
that port wait for it instead of failing.

## Requirements

- Linux or macOS
- R with the `shiny` package, and `Rscript` on your `PATH`
- Go 1.26.5 or newer to install. There are no prebuilt binaries yet.

## Install

```sh
go install github.com/spriyansh/shinyctl@latest
```

Or from a clone of this repo:

```sh
make install
```

## Quick start

```sh
shinyctl init                     # writes ./shinyctl.yaml
$EDITOR shinyctl.yaml             # set entrypoint-file and port
shinyctl apply -f shinyctl.yaml   # starts shinyctl-api, then the app
shinyctl status
```

```
NAME    PORT  PID    STATUS  READY  MEM   CRASHES  STARTED
my-app  8080  61429  alive   ready  142M  -        2026-10-11T20:20:21+02:00
```

## Try the example

```sh
shinyctl apply -f examples/hello/shinyctl.yaml
```

Open http://127.0.0.1:8765 and click **Crash this app**. `shinyctl status`
shows `crash-looping` for a couple of seconds, then a new process. The
page reloads by itself and shows the new process ID.
`shinyctl logs -f examples/hello/shinyctl.yaml` shows what the app printed,
including the crash. Remove the app with
`shinyctl delete -f examples/hello/shinyctl.yaml`.

## Commands

| Command | What it does |
|---|---|
| `init` | Writes a starter manifest. `-o` sets the path, `--force` overwrites. |
| `apply -f <file>` | Starts the app, or updates it if the manifest changed (see below). Starts `shinyctl-api` on first use. Also resumes a stopped app. |
| `status` | Lists every app `shinyctl-api` manages. Never starts `shinyctl-api`. |
| `stop -f <file>` | Sends SIGTERM to the app's own process, waits 10 seconds, then SIGKILL, and closes the app's port. The app stays down until the next `apply` or `restart`. |
| `restart -f <file>` | Stops the app's own process and starts it again from the manifest as it is now, so new code in `app.R` and other app files takes effect. Also starts a stopped or crash-looping app right away. |
| `logs -f <file>` | Prints the last 100 lines of the app's log. `--follow` keeps printing until Ctrl-C, `-n` sets the number of lines, `--api` shows `shinyctl-api`'s log instead. Never starts `shinyctl-api`. |
| `delete -f <file>` | Stops the app, forgets it and removes its state file. Logs are kept. |
| `startup` | Prints a systemd user unit that starts `shinyctl-api` at boot (Linux), and on stderr how to install it. |
| `serve` | Runs `shinyctl-api` in the foreground. `-d` runs it in the background. |
| `version` | Prints the version. Source builds print `dev`. |

`apply` restarts a running app when a manifest field that shapes its
process changed: `app-group`, `app-name`, `entrypoint-file`, `working-dir`
or `env`. A changed `port`, `host` or `restart-policy` takes effect without
restarting the app's process. `apply` never looks at `app.R` or other app
files: run `restart` for code changes.

`stop`, `restart`, `delete` and a re-apply signal a process only if its
command line contains the app's `entrypoint-file`. If the recorded process
ID now belongs to something else, shinyctl leaves it alone, and `stop`,
`restart` or `delete` names it in the reply.

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
replicas: 1                     # optional; 1 is the only accepted value
```

- **Relative paths resolve from the manifest's folder**, so commands work
  from any directory. `~` is not expanded.
- **`host` must be an IP address**, not a hostname. `0.0.0.0` listens on
  IPv4 only and `::` on IPv6 only.
- **`port` can't be between 20000 and 29999.** shinyctl runs each app's R
  process on a port in that range, on 127.0.0.1, behind the load balancer
  that listens on `port`. Your app sees requests coming from 127.0.0.1; the
  visitor's address is the first entry of its `X-Forwarded-For` header. When
  a web server on the same machine in front of the port (nginx terminating
  TLS, say) sends its own `X-Forwarded-For`, `X-Forwarded-Proto` and
  `X-Forwarded-Host`, they reach the app, with 127.0.0.1 added to the end of
  `X-Forwarded-For`. Those headers from anywhere else are replaced.
- **`shinyctl-api` inherits the environment of the shell that started
  it.** Apps get that environment plus `env`.

## Restarts

- `always` restarts after any exit. `on-failure` restarts only after a
  non-zero exit. `never` leaves the app down.
- A crash is noticed the moment it happens. A check every 5 seconds
  catches anything else.
- Repeated crashes back off: 2 seconds, doubling up to 1 minute. The count
  resets once the app stays up for a minute.
- `STATUS` in `status` is `alive`, `dead`, `stopped` or `crash-looping`.
  `READY` turns from `starting` to `ready` once the app's R process
  accepts connections and the load balancer serves its port. `MEM` is the
  resident memory of the app's main R process; worker processes it started
  aren't counted. The load balancer's own memory, about 40 MB for all apps
  together, isn't counted in any app.
- While an app restarts, a request to its port waits up to 15 seconds for
  it. After that the visitor gets a page saying the app is restarting,
  which reloads itself every few seconds.
- After a restart, an open page greys out until it is reloaded. The
  example reloads it by itself once the app answers again: copy its
  `tags$script` into your app's UI. Shiny's own
  `session$allowReconnect("force")` is no substitute: with shiny 1.14, a
  page reconnected to a new R process keeps its old outputs, and its
  downloads stop working.

## Survive reboots (Linux)

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

## Stopping everything

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

## Files

Everything lives under `~/.shinyctl/`:

| Path | Contents |
|---|---|
| `api.log` | `shinyctl-api`'s own log, and the load balancer's |
| `logs/<app-group>-<app-name>.log` | The app's stdout and stderr |
| `registry.json` | Every app `shinyctl-api` manages |
| `api.sock` | The socket the CLI talks to |
| `proxy/` | The load balancer's settings, lock and socket |

Logs rotate at 1 GiB, keeping one previous copy as `.log.1`.

## Limitations

- **A changed port that's in use fails the apply**, and the app stays on
  its old port. Changing `app-state-file` makes it a different app, so
  `delete` the old one with its old manifest first.
- **Apps keep running if `shinyctl-api` dies, but nothing watches them.**
  Run `shinyctl serve -d` or any `apply` to bring it back. It resumes every
  app it was managing. On Linux, [the unit above](#survive-reboots-linux)
  starts it at boot and restarts it whenever it dies. The load balancer
  keeps serving them, but if it dies too, every app is unreachable until
  `shinyctl-api` is back.
- **Logging out can kill everything on systemd servers.** Enable
  lingering with `loginctl enable-linger $USER` if your server allows it.
  `shinyctl-api` writes a warning to `api.log` when lingering is off.
- **`stop` signals only the main R process.** Worker processes the app
  started, for example through callr or future, can outlive it.
- **One process per app, for now.** A restart drops every open session and
  pays the app's full startup cost again; requests wait for it, and pages
  with the example's script reload.
- **The load balancer is in the path of every request.** If it dies,
  `shinyctl-api` restarts it within about a second, and every app is
  unreachable until then. Upgrading shinyctl restarts it once, when
  `shinyctl-api` next starts. The first upgrade to a version with the load
  balancer also restarts every app once.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development and releases.

## License

MIT. See [LICENSE](LICENSE).
