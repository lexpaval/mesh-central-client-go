.PHONY: build build-all release gui gui-linux gui-windows gui-macos gui-all gui-release gui-test gui-shots gui-readme-shots gui-bench gui-soak clean version

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
# first use: Linux amd64/arm64 AppImages (glibc 2.39+), packed with the
# latest continuous appimagetool and runtime, downloaded per build, Windows
# amd64/arm64 static exes as a .zip and a per-user .msi installer that also
# puts the mcc CLI on PATH, macOS 15+ arm64/x86_64 as .dmg images of the .app
# with Qt and the mcc CLI inside, ad-hoc signed. gui-all and gui-release build
# all of them.
GUI_LDFLAGS := -ldflags "-s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(DATE)"
# The build images, mcc-gui-build:<platform>-qt<QT_VERSION>, all with the
# same released Qt. One is built when its tag is missing, so a changed
# Dockerfile (or a file it copies) needs a new tag: another Qt, or IMAGE_REV.
# Linux installs QT_VERSION; MSYS2 and MacPorts only serve their newest Qt,
# so the Windows and macOS builds stop when that isn't QT_VERSION.
# Old images: podman images mcc-gui-build, podman rmi.
QT_VERSION := 6.12.0
IMAGE_REV :=
LINUX_IMAGE := linux-qt$(QT_VERSION)$(IMAGE_REV)
WINDOWS_IMAGE := windows-qt$(QT_VERSION)$(IMAGE_REV)
MACOS_IMAGE := macos-qt$(QT_VERSION)$(IMAGE_REV)
# The build cache is per tag: cgo caches by flags, not Qt's headers, so
# another Qt starts over. Old ones: podman volume prune.
GUI_RUN = podman run --rm --security-opt label=disable -v $(CURDIR):/src -w /src \
	-v mcc-gomod:/root/go/pkg/mod -v mcc-gui-gocache-$(1):/root/.cache/go-build \
	-e GOTOOLCHAIN=auto -e GOFLAGS=-buildvcs=false \
	-e VERSION=$(VERSION) -e COMMIT=$(COMMIT) -e DATE=$(DATE) -e APP_VERSION=$(APP_VERSION) -e APP_BUILD=$(APP_BUILD) \
	mcc-gui-build:$(1) sh mcc-gui/package/build.sh
GUI_IMAGE = podman image exists mcc-gui-build:$(1) || \
	podman build $(2) -t mcc-gui-build:$(1) -f mcc-gui/package/$(3).Dockerfile mcc-gui/package

gui:
	mkdir -p dist
	go build $(GUI_LDFLAGS) -o dist/mcc-gui ./mcc-gui

gui-test:
	go test -count=1 ./mcc-gui

# Connect an offscreen test GUI to the dummy server and capture both themes.
gui-shots:
	mkdir -p dist/shots
	go test -c -o dist/gui-shots ./mcc-gui
	go run ./tools/guibench -screenshots dist/shots ./dist/gui-shots

gui-readme-shots: gui-shots
	mkdir -p docs/screenshots
	cp dist/shots/light.png dist/shots/dark.png dist/shots/light-shell.png dist/shots/dark-shell.png \
		dist/shots/light-files.png dist/shots/dark-files.png dist/shots/light-details.png dist/shots/dark-details.png docs/screenshots/

gui-linux:
	$(call GUI_IMAGE,$(LINUX_IMAGE),--build-arg QT_VERSION=$(QT_VERSION),linux)
	$(call GUI_RUN,$(LINUX_IMAGE)) linux amd64 package
	$(call GUI_RUN,$(LINUX_IMAGE)) linux arm64 package

gui-windows:
	$(call GUI_IMAGE,$(WINDOWS_IMAGE),--build-arg QT_VERSION=$(QT_VERSION),windows)
	$(call GUI_RUN,$(WINDOWS_IMAGE)) windows amd64 package
	$(call GUI_RUN,$(WINDOWS_IMAGE)) windows arm64 package

gui-macos:
	$(call GUI_IMAGE,$(MACOS_IMAGE)-arm64,--build-arg TARGET_ARCH=arm64 --build-arg QT_VERSION=$(QT_VERSION),macos)
	$(call GUI_IMAGE,$(MACOS_IMAGE)-x86_64,--build-arg TARGET_ARCH=x86_64 --build-arg QT_VERSION=$(QT_VERSION),macos)
	$(call GUI_RUN,$(MACOS_IMAGE)-arm64) macos arm64
	$(call GUI_RUN,$(MACOS_IMAGE)-x86_64) macos x86_64

gui-all: gui-linux gui-windows gui-macos

gui-release:
	$(call GUI_IMAGE,$(LINUX_IMAGE),--build-arg QT_VERSION=$(QT_VERSION),linux)
	$(call GUI_IMAGE,$(WINDOWS_IMAGE),--build-arg QT_VERSION=$(QT_VERSION),windows)
	$(call GUI_IMAGE,$(MACOS_IMAGE)-arm64,--build-arg TARGET_ARCH=arm64 --build-arg QT_VERSION=$(QT_VERSION),macos)
	$(call GUI_IMAGE,$(MACOS_IMAGE)-x86_64,--build-arg TARGET_ARCH=x86_64 --build-arg QT_VERSION=$(QT_VERSION),macos)
	$(call GUI_RUN,$(LINUX_IMAGE)) linux amd64 package
	$(call GUI_RUN,$(LINUX_IMAGE)) linux arm64 package
	$(call GUI_RUN,$(WINDOWS_IMAGE)) windows amd64 package
	$(call GUI_RUN,$(WINDOWS_IMAGE)) windows arm64 package
	$(call GUI_RUN,$(MACOS_IMAGE)-arm64) macos arm64
	$(call GUI_RUN,$(MACOS_IMAGE)-x86_64) macos x86_64

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
		$(foreach arch,amd64 arm64,mcc-gui-linux-$(arch)-$(VERSION).AppImage mcc-gui-linux-$(arch)-$(VERSION).AppImage.zsync mcc-gui-windows-$(arch)-$(VERSION).zip mcc-gui-windows-$(arch)-$(VERSION).msi mcc-gui-darwin-$(arch)-$(VERSION).dmg) > sha256sums.txt

version:
	@echo "Version:    $(VERSION)"
	@echo "Git Commit: $(COMMIT)"
	@echo "Build Date: $(DATE)"

clean:
	rm -f mcc
	rm -rf dist/ .cache/ .config/
