FROM rust:1.88-bullseye

ARG GO_VERSION=1.24.4
ARG TARGET_TRIPLE=aarch64-unknown-linux-gnu

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        binutils-aarch64-linux-gnu \
        ca-certificates \
        cmake \
        curl \
        file \
        g++-aarch64-linux-gnu \
        gcc-aarch64-linux-gnu \
        libc6-dev-arm64-cross \
        protobuf-compiler \
        wget \
    && rm -rf /var/lib/apt/lists/*

RUN rustup target add ${TARGET_TRIPLE}

RUN wget -q "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -O /tmp/go.tgz \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz

ENV PATH="/usr/local/go/bin:${PATH}" \
    CC_aarch64_unknown_linux_gnu="aarch64-linux-gnu-gcc" \
    CXX_aarch64_unknown_linux_gnu="aarch64-linux-gnu-g++"

WORKDIR /work

CMD ["/bin/bash"]
