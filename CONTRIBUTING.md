# Contributing

## Developing

Everything goes through the Makefile:

```sh
make format        # go mod tidy + go fmt ./...
make check-format  # fail if any file needs gofmt
make vet           # go vet ./...
make test          # go test ./...
make test-race     # go test -race -shuffle=on ./...
make install       # vet, then go install .
make run           # format, install, then apply the example app in examples/hello
```

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
`internal/proxy` share the load balancer library's global state and must
never call `t.Parallel`.

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
