.PHONY: build build-all clean version

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -ldflags "\
	-X 'github.com/lexpaval/mesh-central-client-go/cmd.Version=$(VERSION)' \
	-X 'github.com/lexpaval/mesh-central-client-go/cmd.GitCommit=$(COMMIT)' \
	-X 'github.com/lexpaval/mesh-central-client-go/cmd.BuildDate=$(DATE)'"

build:
	go build $(LDFLAGS) -o mcc .

build-all:
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o dist/mcc-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o dist/mcc-linux-arm64 .
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o dist/mcc-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o dist/mcc-darwin-arm64 .
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/mcc-windows-amd64.exe .

version:
	@echo "Version:    $(VERSION)"
	@echo "Git Commit: $(COMMIT)"
	@echo "Build Date: $(DATE)"

clean:
	rm -f mcc
	rm -rf dist/