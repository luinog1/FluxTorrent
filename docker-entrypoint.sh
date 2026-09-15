#!/bin/sh
# FluxTorrent container entrypoint.
#
# Goal: keep the binary a plain PID-1 process (it handles SIGTERM directly for a
# clean shutdown), but make the image boot on every kind of host:
#
#   1. Self-hosted `docker run` (root, bind-mounted /config)        → works as before
#   2. `docker run --user 1000:1000` (root image, dropped privs)    → chown via PUID/PGID
#   3. PaaS/K8s that force runAsNonRoot with a root-owned /config  → fall back to /tmp
#                                                                      (state is ephemeral;
#                                                                       point FT_CONFIG_DIR
#                                                                       at a writable
#                                                                       persistent path to
#                                                                       restore persistence)
#
# The fallback in (3) is what fixes the runxbuild CrashLoopBackOff:
# `open store: open /config/fluxtorrent.db: permission denied` happens because
# the platform runs the container as a non-root UID while the mounted /config is
# root-owned. Redirecting FT_CONFIG_DIR to a writable location lets the binary
# boot; once running, set FT_CONFIG_DIR=<persistent writable path> in the
# platform's env editor to keep state across restarts.
set -e

CONFIG_DIR="${FT_CONFIG_DIR:-/config}"
DOWNLOADS_DIR="${FT_CACHE_PATH:-/downloads}"

# Best-effort: ensure the dirs exist before we probe them.
mkdir -p "$CONFIG_DIR" "$DOWNLOADS_DIR" 2>/dev/null || true

# --- root path: optional privilege drop via PUID/PGID ------------------------
if [ "$(id -u)" = "0" ]; then
    PUID="${PUID:-0}"
    PGID="${PGID:-0}"
    if [ "$PUID" != "0" ] || [ "$PGID" != "0" ]; then
        # Chown the volume mount points to the requested user so files created
        # by the binary (bbolt db, disk cache) are owned by that user on the host
        # when bind-mounted. Errors are tolerated (read-only mounts, etc.).
        chown -R "$PUID:$PGID" "$CONFIG_DIR" "$DOWNLOADS_DIR" 2>/dev/null || true
        # su-exec execs the binary, replacing the shell — PID 1 is preserved.
        exec su-exec "$PUID:$PGID" /usr/local/bin/fluxtorrent "$@"
    fi
    # Running as root with no PUID/PGID: keep the legacy behavior (root binary).
    exec /usr/local/bin/fluxtorrent "$@"
fi

# --- non-root path: writable checks with fallback ----------------------------
# Platforms that force runAsNonRoot (runxbuild, some Render plans, OpenShift
# restricted SCC) may still mount /config owned by root. Probe and redirect.
if [ ! -w "$CONFIG_DIR" ]; then
    fallback="/tmp/fluxtorrent-config"
    echo "fluxtorrent: $CONFIG_DIR is not writable (running as uid $(id -u)); falling back to $fallback" >&2
    echo "fluxtorrent: state will NOT persist across restarts. Set FT_CONFIG_DIR to a writable persistent path on the platform to restore persistence." >&2
    mkdir -p "$fallback"
    CONFIG_DIR="$fallback"
    export FT_CONFIG_DIR="$CONFIG_DIR"
fi
if [ ! -w "$DOWNLOADS_DIR" ]; then
    fallback="/tmp/fluxtorrent-downloads"
    echo "fluxtorrent: $DOWNLOADS_DIR is not writable (running as uid $(id -u)); falling back to $fallback" >&2
    echo "fluxtorrent: disk cache will NOT persist across restarts. Set FT_CACHE_PATH to a writable persistent path on the platform to restore persistence." >&2
    mkdir -p "$fallback"
    DOWNLOADS_DIR="$fallback"
    export FT_CACHE_PATH="$DOWNLOADS_DIR"
fi

exec /usr/local/bin/fluxtorrent "$@"
