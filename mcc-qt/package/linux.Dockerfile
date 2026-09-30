# Builds mcc-qt for Linux amd64 and arm64 against Debian 12's Qt 6.4, the
# oldest Qt 6 in current distributions (Ubuntu 24.04 has 6.4 too), so the
# binaries run with any Qt 6.4+ and glibc 2.36+. arm64 cross-compiles with
# Debian's multiarch Qt. Based on miqt's docker/linux64-go1.26-qt6.4-dynamic.

FROM docker.io/library/debian:bookworm

RUN dpkg --add-architecture arm64 && \
    DEBIAN_FRONTEND=noninteractive apt-get update && \
    apt-get install -qyy --no-install-recommends build-essential pkg-config wget ca-certificates xz-utils \
        qt6-base-dev g++-aarch64-linux-gnu qt6-base-dev:arm64 && \
    apt-get clean

ARG GO_VERSION=1.26.2
RUN wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" && \
    tar x -C /usr/local -f "go${GO_VERSION}.linux-amd64.tar.gz" && \
    rm "go${GO_VERSION}.linux-amd64.tar.gz"

ENV PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ENV CGO_ENABLED=1
