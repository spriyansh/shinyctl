# Contributing

## Developing

Everything goes through the Makefile:

```sh
make format        # go mod tidy + go fmt ./...
make check-format  # fail if any file needs gofmt
make vet           # go vet ./...
make cross         # go vet for linux and darwin, amd64 and arm64
make test          # go test ./...
make test-race     # go test -race -shuffle=on ./...
make install       # vet, then go install .
make run           # format, install, then apply the example app in examples/hello
```

Run `make cross` before sending anything that touches syscalls or
system-specific code: it builds for all four release targets, and a
system you are not on can fail to build.

Run `make test-race` too before sending anything that touches `cmd/` or
`internal/state` — the reconcile loop, the exit-watchers and the HTTP
handlers all run concurrently, and the race detector is what catches an
unsynchronized Load → Save. CI (`.github/workflows/ci.yml`) runs
`check-format`, `vet` and `test-race` on Linux and macOS for pushes to
`main` and pull requests, plus `make cross`, a check of the license notices
and of `.goreleaser.yaml`. `.github/workflows/vuln.yml` runs `govulncheck`
weekly and on pushes; a failure there is a dependency to update, not a broken
change. Dependabot opens one pull request a week for the Go modules and one
for the actions.

To run the suite on Linux from a Mac, start [colima](https://github.com/abiosoft/colima)
(`colima start`) and run it in a container, with the repo mounted read-only:

```sh
mkdir -p ~/.cache/shinyctl-gomod
docker run --rm --init -v "$PWD":/src:ro -v ~/.cache/shinyctl-gomod:/go/pkg/mod -w /src \
  -e GOFLAGS=-buildvcs=false -e GOTOOLCHAIN=auto \
  golang:1.26.5 make check-format vet test-race
```

go.mod asks for a newer Go than the image has, and `GOTOOLCHAIN=auto` makes the
image's Go fetch the one go.mod names (into the mounted cache, so only the first
run downloads it). Any `golang` image of 1.21 or newer works the same way.

On a Raspberry Pi 5, `test-race` stops before any test runs ("unsupported
VMA range"): the race detector needs a 48-bit address space, and the Pi 5
kernel has 47 bits. Run `make test` there.

The `cmd` tests start the load balancer as a real process, the test binary
itself run as `proxy --config`, in a short temp directory, because Unix
socket paths are limited to 103 bytes on macOS. The in-process tests in
`internal/proxy` and `internal/proxy/lb` share the load balancer library's
global state and the copies table. They must never call `t.Parallel`, and
they reset the table with `lb.Set(nil)` in a cleanup.

Every behavior change comes with a test. Its commit message says what
changed for users and why.

## How it fits together

One binary is the command line, `shinyctl-api` (`shinyctl serve`, a background
process) and the load balancer (the hidden `shinyctl proxy`, a process of its
own).

- The CLI reads `shinyctl.yaml` and asks `shinyctl-api` to do things over
  `~/.shinyctl/api.sock` (package `internal/api`).
- `shinyctl-api` owns every app's `state.json` and runs the **cycle**, on a
  5 second tick and whenever something happens (a request, an exit): observe
  the copies (`cmd/reconcile.go`), ask `reconcile.DecideCopies` what to do,
  record it in `state.json`, push the **copies table** to the load balancer,
  then start and stop processes. `DecideCopies` is pure: it reads and writes
  nothing, so its tests are tables.
- The load balancer listens on every app's public port and sends each
  request to a copy by the copies table, which it keeps in memory and which
  `shinyctl-api` replaces over the load balancer's control socket without a
  reload (package `internal/proxy/lb`). Each app's table has three roles:
  `active`, `warm` and `draining`. The settings it starts from are
  `proxy/config.json`; the last table is `proxy/copies.json`.
- The copies run on internal ports (20000-29999) on 127.0.0.1. Three kinds of
  watcher keep the picture current: an exit-watcher per process, a
  readiness check per new copy, and watchers on the processes adopted from an
  earlier `shinyctl-api` (`cmd/reconcile_watch.go`).
- What is **stored** is what happened (processes, start times, crash counts,
  the time a copy began to drain). What is **derived** on every cycle is
  what should happen: roles, what to start, what to stop. Nothing about a role
  is stored.

Words that mean one thing in the code:

| Word | Meaning |
|---|---|
| copy | One R process of an app, numbered from 1. |
| role | `active`, `warm` or `draining`: how the load balancer treats a copy. |
| active | Takes new visitors. There are `replicas` of them. |
| warm | A ready spare. It takes the visitors of an active copy that dies. |
| draining | Keeps the visitors it has, gets no new ones, and stops when they are gone or `drain-timeout` passes. |
| outdated | A copy that runs an older version than the manifest (another revision or other settings). |
| revision | A number in `state.json` that a restart raises, so copies started before it are outdated. |
| switch | The moment a redeploy hands new visitors to the new copies; the old ones then drain. |
| settle | The 10 seconds a draining copy is kept even with no sessions, for a page that opens its WebSocket just after. |
| skip-drain | A mark `--now` puts on a copy: it stops instead of draining. |
| stand-in | A warm copy that serves in place of an active one that is down, before the table changes. |
| successor | The copy that took the place of one that left the table; a visitor pinned to the old one is sent to it. |

## Releasing

Pushing a `v*` tag triggers `.github/workflows/release.yml`, which runs
[GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`) and publishes a
GitHub Release with binaries for `linux`/`darwin` × `amd64`/`arm64`, plus a
`checksums.txt`. Nothing manual beyond the tag itself:

```sh
git tag v0.1.0
git push --tags
```

To sanity-check the GoReleaser config locally before trusting a real tag
push (doesn't need a tag, doesn't need push access, doesn't publish
anything):

```sh
goreleaser build --snapshot --clean
```

`shinyctl version` reports whatever GoReleaser injected for that build;
a plain `go build`/`go install` leaves it at `dev`.
