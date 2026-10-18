# shinyctl

Keeps R Shiny apps running on a single machine. No root, no containers, no
proxy.

You describe an app in a YAML manifest and run `shinyctl apply`. A per-user
background process, `shinyctl-api`, starts the app and restarts it whenever
it dies. Each app runs as one R process on the port you chose.

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
NAME    PORT  PID    STATUS  READY  CRASHES  STARTED
my-app  8080  61429  alive   ready  -        2026-10-11T20:20:21+02:00
```

## Try the example

```sh
cd examples/hello
shinyctl apply -f shinyctl.yaml
```

Open http://127.0.0.1:8765 and click **Crash this app**. `shinyctl status`
shows `crash-looping` for a couple of seconds, then a new process. Reload
the page to see the new process ID. Remove the app with
`shinyctl delete -f shinyctl.yaml`.

## Commands

| Command | What it does |
|---|---|
| `init` | Writes a starter manifest. `-o` sets the path, `--force` overwrites. |
| `apply -f <file>` | Starts the app if it isn't running. Starts `shinyctl-api` on first use. Also resumes a stopped app. |
| `status` | Lists every app `shinyctl-api` manages. Never starts `shinyctl-api`. |
| `stop -f <file>` | Sends SIGTERM to the app's own process, waits 10 seconds, then SIGKILL. The app stays down until the next `apply`. |
| `delete -f <file>` | Stops the app, forgets it and removes its state file. Logs are kept. |
| `serve` | Runs `shinyctl-api` in the foreground. `-d` runs it in the background. |
| `version` | Prints the version. Source builds print `dev`. |

`stop`, `delete` and a re-apply signal a process only if its command line
contains the app's `entrypoint-file`. If the recorded process ID now
belongs to something else, shinyctl leaves it alone, and `stop` or
`delete` names it in the reply.

## Manifest

```yaml
app-group: lab                  # required
app-name: explorer              # required
entrypoint-file: ./app.R        # required
port: 8080                      # required
app-state-file: ./state.json    # required; identifies the app to stop and delete
host: 0.0.0.0                   # default 0.0.0.0 (all interfaces); 127.0.0.1 = this machine only
restart-policy: always          # always (default), on-failure, or never
working-dir: ./                 # default: the entrypoint-file's directory
env:                            # added on top of shinyctl-api's own environment
  R_LIBS_USER: /home/me/Rlibs
replicas: 1                     # optional; 1 is the only accepted value
```

- **Relative paths resolve from your current directory**, not from the
  manifest's location. Run `shinyctl` from the same directory every time,
  or use absolute paths.
- **`host` must be an IP address**, not a hostname.
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
  `READY` turns from `starting` to `ready` once the port accepts
  connections.

## Files

Everything lives under `~/.shinyctl/`:

| Path | Contents |
|---|---|
| `api.log` | `shinyctl-api`'s own log |
| `logs/<app-group>-<app-name>.log` | The app's stdout and stderr |
| `registry.json` | Every app `shinyctl-api` manages |
| `api.sock` | The socket the CLI talks to |

Logs rotate at 1 GiB, keeping one previous copy as `.log.1`.

## Limitations

- **Re-applying only acts on `port` and `app-name` changes.** Changing
  either stops the running copy and starts a new one from the edited
  manifest, so other edits made at the same time take effect too. The old
  copy is stopped before the new port is checked, so a taken port leaves
  the app down. Changing only `env`, `host`, `working-dir` or
  `entrypoint-file` has no effect on a running app: `stop` it, edit the
  manifest, then `apply`. Changing `app-state-file` makes it a different
  app, so `delete` the old one with its old manifest first.
- **Nothing restarts `shinyctl-api` after a reboot.** Apps keep running if
  `shinyctl-api` dies, but nothing watches them. Run `shinyctl serve -d`
  or any `apply` to bring it back. It resumes every app it was managing.
- **Logging out can kill everything on systemd servers.** Enable
  lingering with `loginctl enable-linger $USER` if your server allows it.
  `shinyctl-api` writes a warning to `api.log` when lingering is off.
- **`stop` signals only the main R process.** Worker processes the app
  started, for example through callr or future, can outlive it.
- **One process per app, by design.** There is no proxy and no load
  balancing. A restart drops every open session and pays the app's full
  startup cost again. A proxy would put `shinyctl-api` in the path of
  every request. Today, apps keep serving when it dies.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development and releases.

## License

MIT. See [LICENSE](LICENSE).
