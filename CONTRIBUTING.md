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
`main` and pull requests.

To run the suite on Linux from a Mac, start [colima](https://github.com/abiosoft/colima)
(`colima start`) and run it in a container, with the repo mounted read-only:

```sh
docker run --rm --init -v "$PWD":/src:ro -w /src -e GOFLAGS=-buildvcs=false \
  golang:1.26.5 make check-format vet test-race
```

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
