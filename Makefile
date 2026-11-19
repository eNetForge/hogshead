BINARY   := hogshead
MODULE   := github.com/enetforge/hogshead
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
GOFLAGS  := CGO_ENABLED=0

.PHONY: all build test test-race lint fmt vet ui ui-check clean run cover

all: build

build:
	$(GOFLAGS) go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test ./...

test-race:
	go test -race -count=1 ./...

cover:
	go test -coverpkg=./... -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out | tail -1

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint:
	@which golangci-lint >/dev/null 2>&1 && golangci-lint run ./... || (echo "golangci-lint not installed, running go vet"; go vet ./...)

ui:
	cd ui && npm ci && npm run build

ui-check: ui
	git diff --exit-code internal/ui/dist

run: build
	./bin/$(BINARY) serve --config dev/config.toml

clean:
	rm -rf bin dist coverage.out
