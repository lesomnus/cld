# cld

Runs Claude Code *inside* your devcontainers, automatically.

`cld serve` watches Docker events; when a devcontainer starts, it copies the
claude CLI into the container, seeds onboarding/trust state, and opens a host
tmux session running claude at the workspace root via `docker exec`. Claude is
sandboxed in the container — the container needs nothing preinstalled, not
even tmux. Conversation state is continuously backed up to the host and
restored when the container is recreated.

## Usage

```sh
# The daemon. Provisions every devcontainer it sees (label cld.ignore=true to opt out).
$ cld serve

# List provisioned devcontainers.
$ cld ls
NAME  CONTAINER     STATUS  VERSION
cld   3f9c2a81b04d  ready   2.1.187

# Attach to a devcontainer's claude session.
$ cld it cld
```

See `cld.yaml` for configuration and `plan.md` for the design.

## Development

The devcontainer ships a DinD sidecar; integration tests run against it via
`DOCKER_HOST`.

```sh
$ go test ./...          # unit + integration (DinD)
$ go test -short ./...   # unit only
```
