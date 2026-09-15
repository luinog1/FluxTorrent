# FluxTorrent — multi-stage build:
#   node builds the UI → go cross-compiles & embeds it → tiny alpine runtime.
#
# The UI and Go stages run on the native BUILDPLATFORM and Go cross-compiles to
# the TARGET arch (CGO disabled, pure Go), so multi-arch images build fast
# without QEMU emulation.

# ---------- stage 1: build the React UI (native) ----------
FROM --platform=$BUILDPLATFORM node:22-alpine AS ui
WORKDIR /ui
COPY web/package.json web/package-lock.json* ./
RUN npm install
COPY web/ ./
RUN npm run build

# ---------- stage 2: cross-compile the Go binary (UI embedded) ----------
FROM --platform=$BUILDPLATFORM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# overlay the freshly built UI so go:embed picks it up
COPY --from=ui /ui/dist ./web/dist
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/fluxtorrent ./cmd/fluxtorrent

# ---------- stage 3: runtime (target arch) ----------
FROM alpine:3.20
# su-exec lets the root entrypoint chown volumes and then drop to a non-root
# UID/GID before exec'ing the binary (PID 1 preserved). ca-certificates is
# required for HTTPS trackers / announce.
RUN apk add --no-cache ca-certificates su-exec
COPY --from=build /out/fluxtorrent /usr/local/bin/fluxtorrent
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh \
 && mkdir -p /config /downloads \
 && chmod 1777 /config /downloads
# Runs as root by default so bind-mounted /config and /downloads "just work"
# on self-hosted setups. The entrypoint drops privileges via PUID/PGID when
# set, and falls back to /tmp when the platform forces runAsNonRoot against
# a root-owned /config (PaaS / managed K8s like runxbuild — see entrypoint).
EXPOSE 7001 42069
ENV FT_CONFIG_DIR=/config FT_LISTEN_HOST=0.0.0.0 FT_LISTEN_PORT=7001
VOLUME ["/config", "/downloads"]
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
