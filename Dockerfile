# syntax=docker/dockerfile:1

ARG GO_VERSION=1.26.2

FROM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN mkdir -p /out/data && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/arkiv-storaged ./cmd/arkiv-storaged

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/arkiv-storaged /usr/local/bin/arkiv-storaged
COPY --from=build --chown=nonroot:nonroot /out/data /data

USER nonroot:nonroot
VOLUME ["/data"]
EXPOSE 2704 2705

ENTRYPOINT ["/usr/local/bin/arkiv-storaged"]
CMD ["-data-dir", "/data"]
