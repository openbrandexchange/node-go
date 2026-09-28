# node-go: Open Brand Exchange brand portal (Go edition).
# One image for every host: Docker/Compose, Cloud Run, Fly.io, Railway, Render.
# Litestream is included; it only runs when LITESTREAM_REPLICA_URL is set.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/openbrandexchange/node-go/internal/node.Version=${VERSION}" -o /out/node-go .

FROM alpine:3.22 AS litestream
ARG TARGETARCH
ARG LITESTREAM_VERSION=0.3.13
RUN apk add --no-cache curl && \
    arch="${TARGETARCH:-amd64}" && \
    curl -fsSL "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/litestream-v${LITESTREAM_VERSION}-linux-${arch}.tar.gz" \
      | tar -xz -C /usr/local/bin litestream

FROM alpine:3.22
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
