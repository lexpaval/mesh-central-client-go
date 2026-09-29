.PHONY: build build-all gui-linux gui-windows gui-all clean version

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -ldflags "\
	-X 'github.com/lexpaval/mesh-central-client-go/cmd.Version=$(VERSION)' \
	-X 'github.com/lexpaval/mesh-central-client-go/cmd.GitCommit=$(COMMIT)' \
	-X 'github.com/lexpaval/mesh-central-client-go/cmd.BuildDate=$(DATE)'"

build:
	go build $(LDFLAGS) -o mcc .

# The GUI needs cgo, so it builds in fyne-cross's image: zig is the C compiler
# for every target (bundles mingw for Windows), with multiarch GL/X11/Wayland
# libs for Linux.
# Linux pins glibc 2.36 (Debian 12), the image's own libs need newer symbols
# but aren't shipped, hence --allow-shlib-undefined. The image has an older
# Go, GOTOOLCHAIN=auto fetches the one go.mod asks for.
GUI_IMAGE := docker.io/fyneio/fyne-cross-images:v1.3.2-linux
GUI_RUN := podman run --rm --security-opt label=disable -v $(CURDIR):/src -w /src \
	-v mcc-gomod:/go/pkg/mod -v mcc-gocache:/root/.cache/go-build \
	-e GOTOOLCHAIN=auto -e CGO_ENABLED=1 -e GOFLAGS=-buildvcs=false
GUI_LINUX_CC = -e GOOS=linux -e GOARCH=$(1) -e PKG_CONFIG_PATH=/usr/lib/$(2)-linux-gnu/pkgconfig \
	-e CC="zig cc -target $(2)-linux-gnu.2.36 -isystem /usr/include -L/usr/lib/$(2)-linux-gnu -Wl,--allow-shlib-undefined"
# zig's linker ignores Go's -H=windowsgui, the subsystem flag hides the console window
GUI_WINDOWS_CC = -e GOOS=windows -e GOARCH=$(1) -e CC="zig cc -target $(2)-windows-gnu -Wdeprecated-non-prototype -Wl,--subsystem,windows"

gui-linux:
	mkdir -p dist
	$(GUI_RUN) $(call GUI_LINUX_CC,amd64,x86_64) $(GUI_IMAGE) go build -o dist/mcc-gui-linux-amd64-$(VERSION) ./gui
	$(GUI_RUN) $(call GUI_LINUX_CC,arm64,aarch64) $(GUI_IMAGE) go build -o dist/mcc-gui-linux-arm64-$(VERSION) ./gui

gui-windows:
	mkdir -p dist
	$(GUI_RUN) $(call GUI_WINDOWS_CC,amd64,x86_64) $(GUI_IMAGE) go build -o dist/mcc-gui-windows-amd64-$(VERSION).exe ./gui
	$(GUI_RUN) $(call GUI_WINDOWS_CC,arm64,aarch64) $(GUI_IMAGE) go build -o dist/mcc-gui-windows-arm64-$(VERSION).exe ./gui

gui-all: gui-linux gui-windows

build-all:
	GOOS=linux   GOARCH=amd64 go build $(LDFLAGS) -o dist/mcc-linux-amd64-$(VERSION) .
	GOOS=linux   GOARCH=arm64 go build $(LDFLAGS) -o dist/mcc-linux-arm64-$(VERSION) .
	GOOS=darwin  GOARCH=amd64 go build $(LDFLAGS) -o dist/mcc-darwin-amd64-$(VERSION) .
	GOOS=darwin  GOARCH=arm64 go build $(LDFLAGS) -o dist/mcc-darwin-arm64-$(VERSION) .
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o dist/mcc-windows-amd64-$(VERSION).exe .
	GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o dist/mcc-windows-arm64-$(VERSION).exe .

release: build-all
	cd dist && sha256sum mcc-linux-amd64-$(VERSION) mcc-linux-arm64-$(VERSION) mcc-darwin-amd64-$(VERSION) mcc-darwin-arm64-$(VERSION) mcc-windows-amd64-$(VERSION).exe mcc-windows-arm64-$(VERSION).exe > sha256sums.txt

version:
	@echo "Version:    $(VERSION)"
	@echo "Git Commit: $(COMMIT)"
	@echo "Build Date: $(DATE)"

clean:
	rm -f mcc
	rm -rf dist/