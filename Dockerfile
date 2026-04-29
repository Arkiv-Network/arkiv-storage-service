# syntax=docker/dockerfile:1.7

ARG UBUNTU_VERSION=26.04
ARG GO_VERSION=1.26.0

FROM ubuntu:${UBUNTU_VERSION} AS go-toolchain

ARG GO_VERSION
ARG TARGETOS=linux
ARG TARGETARCH=amd64

ENV CGO_ENABLED=0 \
    GOPATH=/go \
    PATH=/usr/local/go/bin:/go/bin:$PATH

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gzip tar \
    && update-ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) go_arch="amd64" ;; \
      arm64) go_arch="arm64" ;; \
      arm) go_arch="armv6l" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.${TARGETOS}-${go_arch}.tar.gz" -o /tmp/go.tgz; \
    tar -C /usr/local -xzf /tmp/go.tgz; \
    rm /tmp/go.tgz; \
    go version

WORKDIR /src

FROM go-toolchain AS deps

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

FROM deps AS production-build

ARG TARGETOS=linux
ARG TARGETARCH=amd64

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out && \
    GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -ldflags="-s -w" -o /out/arkiv-storaged ./cmd/arkiv-storaged

FROM deps AS integration-build

ARG TARGETOS=linux
ARG TARGETARCH=amd64

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    mkdir -p /out && \
    GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -o /out/arkiv-storaged ./cmd/arkiv-storaged

FROM ubuntu:${UBUNTU_VERSION} AS runtime-base

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && update-ca-certificates \
    && groupadd --system docker \
    && useradd --system --create-home --gid docker --home-dir /home/docker --shell /usr/sbin/nologin docker \
    && mkdir -p /var/lib/arkiv-storaged \
    && chown -R docker:docker /var/lib/arkiv-storaged /home/docker \
    && rm -rf /var/lib/apt/lists/*

EXPOSE 2704 2705
USER docker

ENTRYPOINT ["arkiv-storaged"]
CMD ["-data-dir", "/var/lib/arkiv-storaged"]

FROM runtime-base AS production

COPY --from=production-build /out/arkiv-storaged /usr/local/bin/arkiv-storaged

FROM runtime-base AS integration

COPY --from=integration-build /out/arkiv-storaged /usr/local/bin/arkiv-storaged
