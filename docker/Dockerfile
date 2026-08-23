# Builder image for the GCC cross-compiler build farm.
# Native (build) triple inside this image on Apple Silicon: aarch64-unknown-linux-gnu
FROM debian:bookworm

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      autoconf \
      automake \
      bison \
      bzip2 \
      ca-certificates \
      curl \
      file \
      flex \
      g++ \
      gawk \
      gcc \
      git \
      gperf \
      gzip \
      libtool \
      libtool-bin \
      m4 \
      make \
      patch \
      perl \
      pkg-config \
      procps \
      python3 \
      qemu-user-static \
      rsync \
      tar \
      texinfo \
      wget \
      xz-utils \
      zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*

# Go toolchain (pinned per arch; sha256 from https://go.dev/dl/?mode=json)
ARG GO_VERSION=1.26.7
ARG GO_SHA256_arm64=5a4ec883379d51ee9ce1040d5e87f8d35e20387574dd8c947feb01eabc3c1b37
ARG GO_SHA256_amd64=ffb5f8de10c62550dfddab66b36b57030721e0a44a3218e9e1181d7b59f121ca
RUN set -eu; \
    arch="$(dpkg --print-architecture)"; \
    eval "sha=\${GO_SHA256_${arch}:-}"; \
    [ -n "$sha" ] || { echo "no pinned go${GO_VERSION} sha256 for $arch; add GO_SHA256_${arch}" >&2; exit 1; }; \
    curl -fsSLo /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz"; \
    echo "${sha}  /tmp/go.tgz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm /tmp/go.tgz

ENV PATH=/usr/local/go/bin:$PATH
# login shells re-source /etc/profile and would drop the ENV above
RUN echo 'export PATH=/usr/local/go/bin:$PATH' > /etc/profile.d/go.sh

WORKDIR /work
