# node-go: Open Brand Exchange brand portal (Go edition).
# One image for every host: Docker/Compose, Cloud Run, Fly.io, Railway, Render.
# Multi-arch (linux/amd64, linux/arm64); the Go build cross-compiles on the
# build machine. Litestream is included; it only runs when
# LITESTREAM_REPLICA_URL is set. Base images are pinned by digest and kept
# current by Dependabot.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/openbrandexchange/node-go/internal/node.Version=${VERSION}" -o /out/node-go .

FROM --platform=$BUILDPLATFORM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 AS litestream
ARG TARGETARCH=amd64
ARG LITESTREAM_VERSION=0.3.13
# sha256 of the release archives (verified when this version was pinned).
ARG LITESTREAM_SHA256_amd64=eb75a3de5cab03875cdae9f5f539e6aedadd66607003d9b1e7a9077948818ba0
ARG LITESTREAM_SHA256_arm64=9585f5a508516bd66af2b2376bab4de256a5ef8e2b73ec760559e679628f2d59
RUN apk add --no-cache curl && \
    case "$TARGETARCH" in amd64) sum="$LITESTREAM_SHA256_amd64" ;; arm64) sum="$LITESTREAM_SHA256_arm64" ;; *) echo "unsupported $TARGETARCH" >&2; exit 1 ;; esac && \
    curl -fsSL -o /tmp/litestream.tgz "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/litestream-v${LITESTREAM_VERSION}-linux-${TARGETARCH}.tar.gz" && \
    echo "${sum}  /tmp/litestream.tgz" | sha256sum -c - && \
    tar -xzf /tmp/litestream.tgz -C /usr/local/bin litestream

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
ARG VERSION=dev
LABEL org.opencontainers.image.title="node-go" \
      org.opencontainers.image.description="Open Brand Exchange brand portal: Portal for Google Cloud & Docker" \
      org.opencontainers.image.source="https://github.com/openbrandexchange/node-go" \
      org.opencontainers.image.url="https://openbrandexchange.com" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.vendor="Open Brand Exchange Foundation (in formation)" \
      org.opencontainers.image.version="${VERSION}"
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 obx && mkdir -p /data && chown obx /data
COPY --from=build /out/node-go /usr/local/bin/node-go
COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/litestream
COPY deploy/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
ENV OBX_DATA_DIR=/data PORT=8080 OBX_LOG_FORMAT=json
VOLUME /data
EXPOSE 8080
USER obx
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s CMD ["node-go", "healthcheck"]
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["serve"]
