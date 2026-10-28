.PHONY: vet test test-race check-format cross install show-busy-ports tidy format run

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race -shuffle=on ./...

check-format:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "needs gofmt (run make format):"; echo "$$out"; exit 1; fi

# cross vets the code (tests included) for the four systems the releases are
# built for, so a build that only breaks elsewhere is found here.
cross:
	@for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		echo "vet $$t"; GOOS=$${t%/*} GOARCH=$${t#*/} CGO_ENABLED=0 go vet ./... || exit 1; \
	done

install: vet
	go install .

show-busy-ports:
	lsof -nP -iTCP -sTCP:LISTEN

tidy:
	go mod tidy

format: tidy
	go fmt ./...

run: format install
	shinyctl apply -f examples/hello/shinyctl.yaml
