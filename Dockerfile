# syntax=docker/dockerfile:1.7

FROM golang:1.24-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src
COPY go.mod ./
COPY go.sum ./
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go env -w GOPROXY=https://proxy.golang.org,direct GOSUMDB=sum.golang.org && \
    rm -f go.sum || true && \
    go mod tidy && \
    go mod download && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=mod -o /out/volume-ops ./cmd/volume-ops

FROM alpine:3.20
RUN apk add --no-cache haproxy supervisor curl ca-certificates util-linux su-exec \
    && addgroup -S app && adduser -S -G app -H -s /sbin/nologin app
WORKDIR /app
COPY --from=builder /out/volume-ops /usr/local/bin/volume-ops
# Allow baking default rclone image at build time
ARG RCLONE_IMAGE="rclone/rclone:latest"
# Export as runtime default (can be overridden by env)
ENV VOLS3_DEFAULT_RCLONE_IMAGE=${RCLONE_IMAGE}
COPY supervisord.conf /app/etc/supervisord.default.conf
COPY scripts/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh \
    && mkdir -p /app/etc /app/var/log/supervisor /app/var/run \
    && chown -R app:app /app

EXPOSE 8080 8081
# run as root here; entrypoint will drop privileges to app
USER root
ENTRYPOINT ["/entrypoint.sh"]
CMD ["/usr/bin/supervisord", "-c", "/app/etc/supervisord.conf"]

