.PHONY: build build-all release gui gui-linux gui-windows gui-macos gui-all gui-release gui-test gui-shots gui-bench gui-soak clean version

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -ldflags "\
	-X 'github.com/lexpaval/mesh-central-client-go/cmd.Version=$(VERSION)' \
	-X 'github.com/lexpaval/mesh-central-client-go/cmd.GitCommit=$(COMMIT)' \
	-X 'github.com/lexpaval/mesh-central-client-go/cmd.BuildDate=$(DATE)'"

build:
	go build $(LDFLAGS) -o mcc .

APP_VERSION := $(or $(shell echo $(VERSION) | sed -nE 's/^v?([0-9]+\.[0-9]+\.[0-9]+).*/\1/p'),0.0.1)
APP_BUILD := $(shell git rev-list --count HEAD 2>/dev/null || echo 1)

# Qt GUI (mcc-gui, miqt bindings). gui builds for this machine against the
# system Qt 6 (Fedora: qt6-qtbase-devel, Debian/Ubuntu: qt6-base-dev), the
# first build compiles the bindings for a few minutes. gui-linux, gui-windows
# and gui-macos cross-build in podman images from mcc-gui/package, built on
# first use: Linux amd64/arm64 binaries against Qt 6.4 (Debian 12, Ubuntu
# 24.04 and later), Windows amd64/arm64 as static exes (Qt 6.11), macOS 14+ arm64/x86_64
# as zipped .app bundles with Qt inside, ad-hoc signed. gui-all builds all of
# them, gui-release packages them instead: Linux .tar.xz (desktop entry and
# icon, unpacks to /usr/local), Windows .zip, macOS .app.zip.
# The build cache is per image: cgo caches by flags, not Qt's headers, so a
# rebuilt image with another Qt starts over. Old ones: podman volume prune.
GUI_LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(DATE)"
GUI_RUN = podman run --rm --security-opt label=disable -v $(CURDIR):/src -w /src \
	-v mcc-gomod:/root/go/pkg/mod -v mcc-gui-gocache-$(1)-$$(podman image inspect -f '{{.Id}}' mcc-gui-build:$(1) | cut -c1-12):/root/.cache/go-build \
	-e GOTOOLCHAIN=auto -e GOFLAGS=-buildvcs=false \
	-e VERSION=$(VERSION) -e COMMIT=$(COMMIT) -e DATE=$(DATE) -e APP_VERSION=$(APP_VERSION) -e APP_BUILD=$(APP_BUILD) \
	mcc-gui-build:$(1) sh mcc-gui/package/build.sh
GUI_IMAGE = podman image exists mcc-gui-build:$(1) || podman build $(2) -t mcc-gui-build:$(1) -f mcc-gui/package/$(3).Dockerfile mcc-gui/package

gui:
	mkdir -p dist
	go build $(GUI_LDFLAGS) -o dist/mcc-gui ./mcc-gui

gui-test:
	go test -count=1 ./mcc-gui

# Renders the Qt window (dark/light), dialogs and a shell with sample data
# to dist/shots, offscreen.
gui-shots:
	mkdir -p dist/shots
	SHOT_DIR=$(CURDIR)/dist/shots go test -run TestScreenshot -count=1 ./mcc-gui

gui-linux:
	$(call GUI_IMAGE,linux,,linux)
	$(call GUI_RUN,linux) linux amd64
	$(call GUI_RUN,linux) linux arm64

gui-windows:
	$(call GUI_IMAGE,windows,,windows)
	$(call GUI_RUN,windows) windows amd64
	$(call GUI_RUN,windows) windows arm64

gui-macos:
	$(call GUI_IMAGE,macos-arm64,--build-arg TARGET_ARCH=arm64,macos)
	$(call GUI_IMAGE,macos-x86_64,--build-arg TARGET_ARCH=x86_64,macos)
	$(call GUI_RUN,macos-arm64) macos arm64
	$(call GUI_RUN,macos-x86_64) macos x86_64

gui-all: gui-linux gui-windows gui-macos

gui-release:
	$(call GUI_IMAGE,linux,,linux)
	$(call GUI_IMAGE,windows,,windows)
	$(call GUI_IMAGE,macos-arm64,--build-arg TARGET_ARCH=arm64,macos)
	$(call GUI_IMAGE,macos-x86_64,--build-arg TARGET_ARCH=x86_64,macos)
	$(call GUI_RUN,linux) linux amd64 package
	$(call GUI_RUN,linux) linux arm64 package
	$(call GUI_RUN,windows) windows amd64 package
	$(call GUI_RUN,windows) windows arm64 package
	$(call GUI_RUN,macos-arm64) macos arm64
	$(call GUI_RUN,macos-x86_64) macos x86_64

# Exercise the GUI against a dummy MeshCentral server on Linux. BENCH_FLAGS
# passes options to tools/guibench; it also accepts multiple binaries for comparisons.
gui-bench: gui
	go run ./tools/guibench $(BENCH_FLAGS) dist/mcc-gui

# Soak with a large device list and two active shells in headless mutter.
# HIDE= keeps the window shown to the compositor instead of minimized.
SOAK ?= 30m
HIDE ?= -hide
gui-soak: gui
	go run ./tools/guibench -headless $(HIDE) -soak $(SOAK) -warmup 30s -devices 16000 -events 50 -shells 2 $(BENCH_FLAGS) dist/mcc-gui

build-all:
	GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o dist/mcc-linux-amd64-$(VERSION) .
	GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o dist/mcc-linux-arm64-$(VERSION) .
	GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o dist/mcc-darwin-amd64-$(VERSION) .
	GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o dist/mcc-darwin-arm64-$(VERSION) .
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/mcc-windows-amd64-$(VERSION).exe .
	GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o dist/mcc-windows-arm64-$(VERSION).exe .

release: build-all gui-release
	cd dist && sha256sum $(foreach os,linux darwin windows,$(foreach arch,amd64 arm64,mcc-$(os)-$(arch)-$(VERSION)$(if $(filter windows,$(os)),.exe))) \
		$(foreach arch,amd64 arm64,mcc-gui-linux-$(arch)-$(VERSION).tar.xz mcc-gui-windows-$(arch)-$(VERSION).zip mcc-gui-darwin-$(arch)-$(VERSION).app.zip) > sha256sums.txt

version:
	@echo "Version:    $(VERSION)"
	@echo "Git Commit: $(COMMIT)"
	@echo "Build Date: $(DATE)"

clean:
	rm -f mcc
	rm -rf dist/ .cache/ .config/
