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

The daemon runs as your user so the sockets it creates under `~/.cache/cld` are
yours (not root), which is what lets the host `cld` reach it. The image
entrypoint handles ownership and Docker-socket group access; you just pass your
ids (they default to 1000):

```sh
# Start the daemon (pulls ghcr.io/lesomnus/cld:edge). It watches Docker events
# and provisions every devcontainer it sees, except those labelled
# cld.ignore=true or matched by an `ignore:` glob in cld.yaml.
$ CLD_UID=$(id -u) CLD_GID=$(id -g) docker compose up -d

# Copy the cld binary out of the running container onto your host.
$ docker compose cp cld:/cld ~/.local/bin/cld

# The compose file shares ~/.cache/cld, so the host binary talks to the running
# daemon over the same socket. Now start any devcontainer (VS Code "Reopen in
# Container", `devcontainer up`, a .devcontainer compose stack, …).
$ cld ls
NAME  CONTAINER     STATUS  VERSION
myapp 3f9c2a81b04d  ready   2.1.191

$ cld it myapp
```

The host needs no tmux for this: `cld it` asks the daemon where its tmux server
lives and, when the daemon runs in a container, attaches through a `docker
exec` into it — the tmux bundled in the image is the only one involved. (With
the daemon running directly on the host instead, `cld it` uses the host tmux.)

Put `CLD_UID`/`CLD_GID` in a `.env` file next to the compose file so plain
`docker compose up -d` picks them up. If you'd rather not run the host binary
at all, drive the daemon in place instead: `docker compose exec cld cld ls`,
`docker compose exec -it cld cld it myapp`.

## Day-to-day usage

Your claude sessions live inside tmux, but you only need a handful of habits —
no prior tmux experience required.

**The one key binding to remember: `ctrl-b` then `d` (detach).** This is how
you *leave* a session. Claude keeps running in the background; `cld it <name>`
brings you right back to it. Don't exit claude just to go do something else.

| You want to…                        | Do this                                            |
| ----------------------------------- | -------------------------------------------------- |
| Open a devcontainer's claude        | `cld it <name>` (names from `cld ls`)              |
| Leave, keep claude running          | `ctrl-b d`                                         |
| Scroll up through output            | mouse wheel, or `ctrl-b [` + arrows, `q` to exit   |
| See what's running                  | `cld ls`                                           |
| Recover after exiting claude        | `cld it --new <name>`                              |

Things that just work — nothing for you to do:

- **Exited claude by accident?** The conversation was already backed up the
  moment it exited; `cld ls` shows the container as `session-ended`. Run
  `cld it --new <name>` and the new session starts with `--continue`, resuming
  exactly where you left off. (The dead pane doesn't take input — don't try to
  type `claude` into it; `--new` is the way.)
- **Restarted the container?** A fresh session is created automatically and
  resumes the previous conversation.
- **Rebuilt/recreated the container?** Same: conversation state is restored
  from the host backup and resumed. History follows the `name` in your
  devcontainer.json, so it even survives moving the project directory.
- **First time in a brand-new project?** Onboarding and the workspace-trust
  prompt are pre-answered. Login happens once ever: either interactively on
  your first attach, or never — if `auth.oauth_token_file` is configured.

One caution: everything keys off the `name` field in devcontainer.json. Give
each project a distinct name — two projects sharing a name share a
conversation history.

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
