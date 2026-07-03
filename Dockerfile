ARG BUILD_HASH="0000000000000000000000000000000000000000"
ARG BUILD_ID="r0"
ARG APP_VERSION="000000-r0"

FROM golang:1.26 AS base

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_ENABLED=0



FROM base AS test

RUN --mount=type=cache,target=/root/.cache/go-build \
	go test -v -trimpath ./...



FROM base AS builder

ARG BUILD_HASH
ARG BUILD_ID
ARG APP_VERSION
RUN BUILD_HASH=${BUILD_HASH} \
	BUILD_ID=${BUILD_ID} \
	APP_VERSION=${APP_VERSION} \
	./scripts/gen-version-file.sh

ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build \
	mkdir /dist \
	&& GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o /dist/arm64 . \
	&& GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /dist/amd64 . \
	&& "/dist/${TARGETARCH}" version

FROM scratch AS build
COPY --from=builder /dist/ /



FROM alpine:3.20 AS app

# The daemon shells out to tmux and downloads claude over HTTPS, so the runtime
# image needs tmux and CA certificates (a scratch image cannot run `cld serve`).
# setpriv drops privileges in the entrypoint.
RUN apk add --no-cache tmux ca-certificates setpriv

# For `serve`, the entrypoint runs as root only to chown the mounted cache/data
# dirs (Docker creates a missing bind source as root) and to grant the Docker
# socket's group, then drops to PUID:PGID (default 1000). Other commands, and
# `docker exec`, run unchanged. This is what lets the sockets under the mounted
# ~/.cache/cld be owned by the host user, with no pre-created dirs.
COPY --chmod=0755 <<'EOF' /usr/local/bin/docker-entrypoint
#!/bin/sh
set -e
if [ "$1" != "serve" ]; then
	exec cld "$@"
fi
PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
CACHE="${XDG_CACHE_HOME:-/data/cache}"
DATA="${XDG_DATA_HOME:-/data/share}"
mkdir -p "$CACHE/cld" "$DATA/cld"
# Recursive: also heals root-owned leftovers from runs of older images.
chown -R "$PUID:$PGID" "$CACHE" "$DATA"
run_groups="$PGID"
if [ -S /var/run/docker.sock ]; then
	run_groups="$run_groups,$(stat -c '%g' /var/run/docker.sock)"
fi
exec setpriv --reuid "$PUID" --regid "$PGID" --groups "$run_groups" cld "$@"
EOF

ARG TARGETARCH
# The real binary lives at /cld so `docker compose cp cld:/cld <dst>` grabs it
# with minimal typing — and grabs the actual file: docker cp does not follow
# symlinks by default, so the short path must be the original, not a link.
# PATH lookup goes through the symlink.
COPY "${TARGETARCH}" /cld
RUN ln -s /cld /usr/local/bin/cld

ENTRYPOINT ["docker-entrypoint"]
CMD ["--help"]
