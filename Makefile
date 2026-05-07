BIN     := arkiv-storaged
CMD     := ./cmd/arkiv-storaged
OUT     := ./bin/$(BIN)
VERSION_PKG := github.com/Arkiv-Network/arkiv-storage-service/version

TAG        ?= $(shell git describe --tags --abbrev=0 --always 2>/dev/null || echo unknown)
COMMIT     ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
DIRTY      ?= $(shell test -z "$$(git status --porcelain 2>/dev/null)" && echo false || echo true)
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS    := -X '$(VERSION_PKG).Tag=$(TAG)' -X '$(VERSION_PKG).Commit=$(COMMIT)' -X '$(VERSION_PKG).Dirty=$(DIRTY)' -X '$(VERSION_PKG).BuildTime=$(BUILD_TIME)'

.PHONY: build install test lint clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(OUT) $(CMD)

install:
	go install -ldflags "$(LDFLAGS)" $(CMD)

test: build
	go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf ./bin
