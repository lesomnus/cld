# cld

Runs Claude Code *inside* your devcontainers, automatically.

`cld serve` watches Docker events; when a devcontainer starts, it copies the
claude CLI into the container, seeds onboarding/trust state, and opens a host
tmux session running claude at the workspace root via `docker exec`. Claude is
sandboxed in the container — the container needs nothing preinstalled, not
even tmux. Conversation state is continuously backed up to the host and
restored when the container is recreated.

You install nothing in your devcontainers: cld copies `claude` (and itself, as
a file watcher) into each one. Inside a provisioned container you can just run
`claude` directly.

## Quick start

The compose file runs the daemon from the published image (which also bundles
the `cld` binary), so you don't build anything. Start it, then copy the binary
out of the running container to drive it from your host — no Go, no build.

```sh
# Start the daemon (pulls ghcr.io/lesomnus/cld:edge). It watches Docker events
# and provisions every devcontainer it sees, except those labelled
# cld.ignore=true or matched by an `ignore:` glob in cld.yaml.
$ docker compose up -d

# Copy the cld binary out of the running container onto your host.
$ docker compose cp cld:/usr/local/bin/cld /usr/local/bin/cld

# The compose file shares ~/.cache/cld, so the host binary talks to the running
# daemon over the same socket. Now start any devcontainer (VS Code "Reopen in
# Container", `devcontainer up`, a .devcontainer compose stack, …).
$ cld ls
NAME  CONTAINER     STATUS  VERSION
myapp 3f9c2a81b04d  ready   2.1.191

$ cld it myapp
```

The daemon container runs as root, so the shared socket is root-owned; if a
host `cld` command hits a permission error, run it with `sudo` — or skip the
copy and drive the daemon in place: `docker compose exec cld cld ls`,
`docker compose exec -it cld cld it myapp`.

## Configuration

All settings are optional; see `cld.yaml` for the full list with defaults.

To drop the `~/.claude` bind mount from your devcontainers entirely, point
`auth.oauth_token_file` at a file holding a token from `claude setup-token`;
cld injects it so a fresh container authenticates with no interactive login.

See `plan.md` for the design and roadmap.

## Development

The dev container ships a Docker-in-Docker sidecar; integration tests run
against it via `DOCKER_HOST`.

```sh
$ go test ./...            # unit + integration (DinD)
$ go test -short ./...     # unit only
$ CLD_E2E_REAL=1 go test ./internal/daemon/ -run TestRealClaudeInstall  # real download
```
