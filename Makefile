VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= $(HOME)/.local
LDFLAGS := -s -w -X github.com/sernik77/CR0N0S/internal/cli.Version=$(VERSION)

.PHONY: build test install clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o cr0 ./cmd/cr0

test:
	go vet ./...
	go test ./...

install: build
	install -Dm755 cr0 $(PREFIX)/bin/cr0

clean:
	rm -f cr0
