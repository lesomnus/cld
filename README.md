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

`cld` is one binary that both drives the daemon and runs as it. Get it — a
release build, `go install github.com/lesomnus/cld@latest`, or copy it out of
the image:

```sh
$ docker create --name cld-tmp ghcr.io/lesomnus/cld:edge \
    && docker cp cld-tmp:/cld ~/.local/bin/cld && docker rm cld-tmp
```

Then bring the daemon up and attach:

```sh
# Run the daemon as a container on your Docker. It mounts the Docker socket and
# your ~/.cache/cld + ~/.local/share/cld, and runs as your user so the sockets
# it creates are yours (which is what lets the host `cld` reach it). Idempotent;
# `--recreate` replaces it, `cld uninstall` removes it.
$ cld install

# The daemon watches Docker events and provisions every devcontainer it sees
# (except those labelled cld.ignore=true or matched by an `ignore:` glob in
# cld.yaml). Start any devcontainer (VS Code "Reopen in Container",
# `devcontainer up`, a .devcontainer compose stack, …), then:
$ cld ls
NAME  CONTAINER     STATUS  VERSION
myapp 3f9c2a81b04d  ready   2.1.191

$ cld it myapp
```

Don't have a devcontainer running yet? `cld up [path]` creates/starts one and
attaches when it's ready:

```sh
$ cld up ~/src/myapp          # or `cld up` in the project directory
```

It runs the official `devcontainer up` — using a `devcontainer` binary or
`npx` on your host if present, otherwise a containerized copy of the CLI
(`ghcr.io/lesomnus/cld:runner`, pulled on first use) so Docker is the only
requirement. Extra flags pass through: `cld up . -- --remove-existing-container`.
(The containerized runner needs a local engine; with a remote `DOCKER_HOST`,
install the devcontainer CLI or Node on your host.)

`cld down <name>` is the inverse: it takes a final backup, then stops and
removes the devcontainer — for a Compose-based devcontainer the whole project
(the dev service plus sidecars) is removed. Named volumes and the host-side
conversation backup are kept, so `cld up` later restores the history.

The host needs no tmux for this: `cld it` asks the daemon where its tmux server
lives and, when the daemon runs in a container, attaches through a `docker
exec` into it — the tmux bundled in the image is the only one involved. (With
the daemon running directly on the host instead, `cld it` uses the host tmux.)

### Running the daemon another way

`cld install` just runs `ghcr.io/lesomnus/cld:edge` with `serve` on your Docker.
To run it yourself — for debugging, a custom setup, or without the `cld install`
step — the repo's `docker-compose.yaml` is kept current as a reference:

```sh
$ CLD_UID=$(id -u) CLD_GID=$(id -g) docker compose up -d
```

You can also run the daemon directly on the host with `cld serve` (it needs
Docker access; attaching from *inside* a devcontainer, though, requires the
containerized daemon). Or drive an in-container daemon in place, no host binary
needed: `docker compose exec cld cld ls`, `docker compose exec -it cld cld it myapp`.

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
| Remove a devcontainer               | `cld down <name>` (keeps the conversation backup)  |

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

**Git inside a session** works like VS Code Dev Containers: your `~/.gitconfig`
is copied in and your host ssh-agent is relayed (`SSH_AUTH_SOCK`), so signed
commits and SSH pushes just work while you're attached. Prefer SSH remotes: a
host-only `credential.helper` (e.g. `gopass`, `osxkeychain`) is *not* forwarded
— it wouldn't exist in the container — so HTTPS auth falls back to whatever the
container itself provides. Turn the agent off with `auth.forward_agent: false`.

**From inside the container**, `cld it` works too — in a VS Code/Cursor terminal,
`docker exec`, etc. With no name it attaches to that container's own session, and
cld pre-installs a **claude** terminal profile so you can open it straight from
the terminal `+` dropdown. (Needs the daemon running in a container; on by
default, disable with `auth.remote_control: false`.)

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
