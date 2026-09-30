# Cross-builds mcc-qt for Windows amd64 and arm64, each one static .exe,
# with llvm-mingw and MSYS2's static Qt 6 (clang64 and clangarm64, the
# newest Qt MSYS2 has). Arch's pacman installs the MSYS2 packages and their
# dependencies into /msys. Qt is LGPLv3: static linking is fine for this open
# source app, whose source lets users relink.

FROM docker.io/library/archlinux:latest

RUN pacman -Syu --noconfirm --needed base-devel pkgconf wget ca-certificates zip && pacman -Scc --noconfirm

ARG LLVM_MINGW=20260922
RUN wget -q "https://github.com/mstorsjo/llvm-mingw/releases/download/${LLVM_MINGW}/llvm-mingw-${LLVM_MINGW}-ucrt-ubuntu-22.04-x86_64.tar.xz" && \
    mkdir /llvm-mingw && tar -xJf llvm-mingw-*.tar.xz -C /llvm-mingw --strip-components=1 && rm llvm-mingw-*.tar.xz

RUN printf '[options]\nArchitecture = any\nSigLevel = Never\n[clang64]\nServer = https://repo.msys2.org/mingw/clang64/\n[clangarm64]\nServer = https://repo.msys2.org/mingw/clangarm64/\n' > /msys.conf && \
    mkdir -p /msys/var/lib/pacman /tmp/msyscache && \
    pacman --config /msys.conf --root /msys --dbpath /msys/var/lib/pacman --cachedir /tmp/msyscache -Sy --noconfirm --noscriptlet \
        $(for a in x86_64 aarch64; do for p in qt6-static zlib libb2 pcre2 libpng harfbuzz freetype bzip2 brotli glib2 gettext-runtime libiconv graphite2; do \
            echo mingw-w64-clang-$a-$p; done; done) && \
    rm -rf /tmp/msyscache

COPY msys-pc.sh /
RUN sh /msys-pc.sh /msys/clang64 && sh /msys-pc.sh /msys/clangarm64 && \
    PKG_CONFIG_PATH=/msys/clangarm64/qt6-static/lib/pkgconfig pkg-config --modversion Qt6Widgets

ARG GO_VERSION=1.26.2
RUN wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" && \
    tar x -C /usr/local -f "go${GO_VERSION}.linux-amd64.tar.gz" && rm "go${GO_VERSION}.linux-amd64.tar.gz"

ENV PATH=/usr/local/go/bin:/llvm-mingw/bin:/usr/local/sbin:/usr/local/bin:/usr/bin
ENV CGO_ENABLED=1 GOOS=windows
# build.sh picks the architecture: GOARCH, CC, CXX, PKG_CONFIG_PATH, CGO_LDFLAGS.
