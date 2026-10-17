# Contributing

## Developing

Everything goes through the Makefile:

```sh
make format   # go mod tidy + go fmt ./...
make vet      # go vet ./...
make test     # go test ./...
make install  # vet, then go install .
make run      # format, install, then apply the example app in examples/hello
```

Run `go test -race ./...` too before sending anything that touches
`cmd/` or `internal/state` — the reconcile loop, the exit-watchers and the
HTTP handlers all run concurrently, and the race detector is what catches
an unsynchronized Load → Save.

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
