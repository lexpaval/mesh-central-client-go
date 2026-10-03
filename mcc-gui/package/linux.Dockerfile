# Qt 6.11 SDKs on Ubuntu 24.04 keep the AppImage baseline at glibc 2.39.
# Both architectures are built on amd64; arm64 uses the cross compiler.
FROM docker.io/library/ubuntu:24.04

RUN dpkg --add-architecture arm64 && \
    sed -i '/^Types: deb/a Architectures: amd64' /etc/apt/sources.list.d/ubuntu.sources && \
    printf 'deb [arch=arm64] http://ports.ubuntu.com/ubuntu-ports noble main universe\ndeb [arch=arm64] http://ports.ubuntu.com/ubuntu-ports noble-updates main universe\ndeb [arch=arm64] http://ports.ubuntu.com/ubuntu-ports noble-security main universe\n' > /etc/apt/sources.list.d/arm64.list && \
    apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -qyy --no-install-recommends \
        build-essential g++-aarch64-linux-gnu pkg-config wget ca-certificates xz-utils \
        python3-venv pax-utils patchelf file fonts-dejavu-core xkb-data \
        libgl-dev libgl-dev:arm64 libegl-dev libegl-dev:arm64 \
        libgtk-3-0t64 libgtk-3-0t64:arm64 libxkbcommon-x11-0 libxkbcommon-x11-0:arm64 \
        libxcb-cursor0 libxcb-cursor0:arm64 libxcb-icccm4 libxcb-icccm4:arm64 \
        libxcb-keysyms1 libxcb-keysyms1:arm64 libxcb-shape0 libxcb-shape0:arm64 \
        libxcb-xinerama0 libxcb-xinerama0:arm64 libxcb-xkb1 libxcb-xkb1:arm64 \
        libdbus-1-3 libdbus-1-3:arm64 libsm6 libsm6:arm64 && apt-get clean

ARG QT_VERSION=6.11.3
RUN python3 -m venv /opt/aqt && /opt/aqt/bin/pip install --no-cache-dir aqtinstall==3.3.0 && \
    /opt/aqt/bin/aqt install-qt linux desktop ${QT_VERSION} linux_gcc_64 --outputdir /opt/qt --archives qtbase qtwayland qtsvg icu && \
    /opt/aqt/bin/aqt install-qt linux_arm64 desktop ${QT_VERSION} linux_gcc_arm64 --outputdir /opt/qt --archives qtbase qtwayland qtsvg icu && \
    ln -s /opt/qt/${QT_VERSION}/gcc_64 /opt/qt/amd64 && \
    ln -s /opt/qt/${QT_VERSION}/gcc_arm64 /opt/qt/arm64 && \
    sed -i 's|^prefix=.*|prefix=/opt/qt/arm64|' /opt/qt/arm64/lib/pkgconfig/*.pc

# Current Type 2 runtime (2026-09-28), with libfuse linked statically.
# Continuous assets can change; verify them rather than silently changing releases.
ARG APPIMAGETOOL_SHA256=a6d71e2b6cd66f8e8d16c37ad164658985e0cf5fcaa950c90a482890cb9d13e0
ARG RUNTIME_AMD64_SHA256=156f4bdbde9c52d01814600013e0a273f0118dc2de98975f3c8c63427ec79074
ARG RUNTIME_ARM64_SHA256=b4ff0030242d0c3bb12ce40541828303cf167493f4793456f0436edd6255c39d
RUN mkdir -p /opt/appimage && cd /opt/appimage && \
    wget -q -O appimagetool https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage && \
    wget -q https://github.com/AppImage/type2-runtime/releases/download/continuous/runtime-x86_64 && \
    wget -q https://github.com/AppImage/type2-runtime/releases/download/continuous/runtime-aarch64 && \
    printf '%s  %s\n' "$APPIMAGETOOL_SHA256" appimagetool "$RUNTIME_AMD64_SHA256" runtime-x86_64 "$RUNTIME_ARM64_SHA256" runtime-aarch64 | sha256sum -c - && \
    chmod +x appimagetool && ./appimagetool --appimage-extract > /dev/null && \
    rm appimagetool

ARG GO_VERSION=1.26.2
RUN wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" && \
    tar x -C /usr/local -f "go${GO_VERSION}.linux-amd64.tar.gz" && \
    rm "go${GO_VERSION}.linux-amd64.tar.gz"

ENV PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ENV CGO_ENABLED=1

# GTK's GNOME preferences backend is loaded dynamically, not linked to Qt.
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -qyy --no-install-recommends \
    dconf-gsettings-backend dconf-gsettings-backend:arm64 && apt-get clean
