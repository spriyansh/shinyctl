.PHONY: vet test install show-busy-ports tidy format run

vet:
	go vet ./...

test:
	go test ./...

install: vet
	go install .

show-busy-ports:
	lsof -nP -iTCP -sTCP:LISTEN

tidy:
	go mod tidy

format: tidy
	go fmt ./...

run: format install
	cd examples/hello && shinyctl apply -f shinyctl.yaml
