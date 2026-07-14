# cld

Runs Claude Code *inside* your devcontainers, in the background — and lets you
attach to it from anywhere.

cld ties a claude session to the devcontainer's lifecycle. When a devcontainer
starts, the daemon copies the claude CLI into it, seeds onboarding/trust state,
and opens a **background** tmux session running claude at the workspace root
(via `docker exec`). claude keeps running whether or not anyone is watching; you
**attach**, **detach**, and **reattach** to it with `cld it` — from your host
**or from a terminal inside the container itself**. When the container stops,
the session goes with it; when it is recreated, the conversation is restored.

Why: a claude agent that lives with the project's container, comes up
automatically, survives you closing your terminal, and is one command to reach
from wherever you happen to be (host shell, a second machine, or the container's
own integrated terminal in VS Code / Cursor).

Nothing is installed in your devcontainers: cld copies `claude` (and itself, as
an in-container helper) into each one — the container needs nothing preinstalled,
not even tmux. Inside a provisioned container you can run `claude` directly, or
`cld it` to attach to the managed background session.

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
# it creates are yours (which is what lets the host `cld` reach it). Re-run with
# `--recreate` to replace it (e.g. to upgrade); `cld uninstall` removes it.
$ cld install

# The daemon watches Docker events and provisions every devcontainer it sees
# (except those labelled cld.ignore=true or matched by an `ignore:` glob in
# cld.yaml). Start any devcontainer (VS Code "Reopen in Container",
# `devcontainer up`, a .devcontainer compose stack, …), then:
$ cld ls
NAME  ALIAS  CONTAINER     STATUS  VERSION  LOCAL FOLDER
myapp myapp  3f9c2a81b04d  ready   2.1.191  ~/src/myapp

$ cld it myapp
```

Don't have a devcontainer running yet? `cld up [path]` creates/starts one and
attaches when it's ready:

```sh
$ cld up ~/src/myapp          # or `cld up` in the project directory
```

It runs the official `devcontainer up` — using a `devcontainer` binary on your
host if present, otherwise a containerized copy of the CLI
(`ghcr.io/lesomnus/cld:runner`, pulled on first use) so Docker is the only
requirement. Extra flags pass through: `cld up . -- --remove-existing-container`.
(The containerized runner needs a local engine; with a remote `DOCKER_HOST`,
install the devcontainer CLI on your host.)

`cld down <name>` is the inverse: it takes a final backup, then stops and
removes the devcontainer — for a Compose-based devcontainer the whole project
(the dev service plus sidecars) is removed, except a sidecar you've marked
`cld.ignore`. Named volumes and the host-side conversation backup are kept, so
`cld up` later restores the history.

`cld purge <name>` goes further: it stops and removes the devcontainer like
`down`, but also deletes its named volumes and its host-side conversation backup,
leaving no trace on the engine or on disk. It is irreversible, so it asks for
confirmation (skip with `-y`). Use `down` to shelve a devcontainer, `purge` to
be rid of it for good.

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

claude runs in the background inside the container; you **attach** to watch or
steer it and **detach** to leave it running. No prior tmux experience is needed —
just one binding: **`ctrl-b` then `d` (detach)**. That leaves claude running;
`cld it` brings you back. Don't quit claude just to step away.

### Two ways to attach

- **From the host** — `cld it <name>` (names from `cld ls`) attaches from
  anywhere: another terminal, a second machine with the same daemon, a script.
  The host needs no tmux or even a claude install; cld reaches the session
  through the daemon.
- **From inside the devcontainer** — the daemon installs `cld` in every
  container, so a terminal *inside* it (a VS Code / Cursor integrated terminal,
  `docker exec`, …) can run `cld it` with **no name** to attach to *that
  container's own* session. cld also pre-installs a **claude** VS Code / Cursor
  terminal profile, so you can open the session straight from the terminal `+`
  dropdown. (In-container attach needs the daemon running as a container — the
  default with `cld install`; disable with `auth.remote_control: false`.)

### A typical session

```sh
$ cld up ~/src/myapp     # create/start the devcontainer; attaches when ready
# ...work with claude...
#   ctrl-b d             # detach — claude keeps running in the background
$ cld it myapp           # later, from the host: reattach where you left off
```

Or open the project in VS Code / Cursor ("Reopen in Container"), then in the
integrated terminal just run `cld it` (or pick the **claude** terminal profile) —
the same background session, no name needed. Close the editor and the session
keeps running; reopen (or `cld it myapp` from the host) to pick it back up.

| You want to…                        | Do this                                            |
| ----------------------------------- | -------------------------------------------------- |
| Open a devcontainer's claude        | `cld it <name>` (names from `cld ls`)              |
| Leave, keep claude running          | `ctrl-b d`                                         |
| Scroll up through output            | mouse wheel, or `ctrl-b [` + arrows, `q` to exit   |
| See what's running                  | `cld ls`                                           |
| Recover after exiting claude        | `cld it --new <name>`                              |
| Remove a devcontainer               | `cld down <name>` (keeps the conversation backup)  |
| Remove every devcontainer cld manages | `cld down --all` (skips `cld.ignore` / non-cld)  |
| Delete a devcontainer for good      | `cld purge <name>` (also deletes volumes + backup) |

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
  prompt are pre-answered. You log in once per project — complete the login on
  your first attach and cld persists it to the project's backup, so every later
  recreate restores it and skips the prompt.

One caution: everything keys off the `name` field in devcontainer.json. Give
each project a distinct name — two projects sharing a name share a
conversation history.

**Git inside a session** works like VS Code Dev Containers: your `~/.gitconfig`
is copied in and your host ssh-agent is relayed (`SSH_AUTH_SOCK`), so signed
commits and SSH pushes just work while you're attached. Prefer SSH remotes: a
host-only `credential.helper` (e.g. `gopass`, `osxkeychain`) is *not* forwarded
— it wouldn't exist in the container — so HTTPS auth falls back to whatever the
container itself provides. Turn the agent off with `auth.forward_agent: false`.

**Your claude config comes with you.** cld installs its own **user-default**
Claude Code config into each session so a devcontainer feels the same
everywhere: `settings.json`, your personal `CLAUDE.md`, and your `commands/`,
`agents/`, and `output-styles/`. This is a directory cld owns
(`~/.local/share/cld/user-default/` by default) — **not** your host's
`~/.claude`; cld never reads or writes that. Populate it by editing files
there directly (copy in whatever you want propagated). `settings.json` is
sanitized first — its secret- and host-only keys are dropped so they never
cross into the container (`env`, the `apiKeyHelper`/`aws*`/`otel` auth
helpers, the project-MCP auto-trust flags), like the git credential helper
above; the rest of what you put there (model, permissions, hooks, presentation
keys) carries over. Credentials, project history, and runtime state are never
propagated this way. It is a mirror, refreshed on each `cld it` / `cld up`
(removing what you removed from user-default); turn it off with
`auth.share_config: false`.

A change made *inside* a container (e.g. installing a skill) is still backed
up — but only into that project's own isolated backup dir, restored on that
project's next `cld up` after a `cld down`. It never becomes the new baseline
for other projects; only editing user-default does that.

## Commands

The daemon (**`cld serve`**) is the engine; everything else is a client of it.
`cld install`/`cld uninstall` set the daemon up as a container; you spend day to
day in `cld up`/`cld it`/`cld ls`/`cld down`.

### Setup

- **`cld install`** — run the daemon as a container on this host's Docker,
  mounting the socket and your shared cache/data dirs as your user. This is the
  normal way to get cld running; do it once per host. `--recreate` replaces an
  existing daemon (e.g. to upgrade the image); `--image` overrides the image.
  Requires a local Docker engine.
- **`cld uninstall`** — stop and remove the daemon container. Conversation
  backups under the data dir are kept, so a later `cld install` + `cld up`
  restores history.
- **`cld serve`** — run the daemon in the foreground, directly (no container).
  For development, debugging, or a host-managed setup (e.g. systemd). `cld
  install` is the containerized equivalent and what most people want; note that
  attaching from *inside* a devcontainer requires the containerized daemon.

### Everyday

- **`cld up [path] [-- extra…]`** — create/start the devcontainer for a project
  and attach when its claude session is ready (`path` defaults to the current
  directory). Runs the official `devcontainer up` (using a `devcontainer`
  on your host, else a containerized copy). `--no-attach` provisions without
  attaching; extra args after `--` pass through to `devcontainer up`. Use it to
  start working on a project.
- **`cld it [name]`** — attach to a devcontainer's background claude session,
  detaching with `ctrl-b d`. With no `name` it picks the only devcontainer —
  which, run *inside* a container, is that container's own session (so a bare
  `cld it` is what the VS Code terminal profile runs). `--new` recreates a
  session you had ended (see below). Your main everyday command.
- **`cld ls`** — list the devcontainers the daemon manages, with each one's
  `NAME`, `ALIAS`, `CONTAINER`, `STATUS` (`provisioning` → `ready`, or
  `session-ended` / `stopped` / `failed`), claude `VERSION`, and `LOCAL FOLDER`
  (the project's path on the host, shown as `~` when under your home). Use it to
  see what's running and to get the names for `cld it`/`cld down`.
- **`cld down <name>`** — take a final backup, then stop and remove the
  devcontainer (for a Compose devcontainer, the whole project, minus any sidecar
  marked `cld.ignore`). Named volumes and the host-side conversation backup are
  kept, so `cld up` later resumes the history. Use it to tear a project down
  without losing its conversation.
  **`cld down --all`** does this for every devcontainer cld manages at once
  (prompting first; `-y`/`--yes` skips it). It only ever touches what cld
  provisioned: before removing each container the daemon re-checks it against the
  same gate, so a container labelled `cld.ignore=true`, one matched by an
  `ignore:` glob, or any non-devcontainer is left alone.
- **`cld purge <name>`** — like `down`, but also deletes the devcontainer's named
  volumes and its host-side conversation backup, so nothing is left behind (the
  shared global state — credentials/settings — is kept). It is irreversible, so
  it prompts first; `-y`/`--yes` skips the prompt. **`cld purge --all`** purges
  every devcontainer cld manages, under the same scope gate as `down --all`.

### Recover / inspect

- **`cld it --new <name>`** — recreate a session you ended (`cld ls` shows it as
  `session-ended`). The new session starts with `--continue`, resuming the prior
  conversation. This is the way back after you *quit* claude (rather than
  detaching) — don't type `claude` into the dead pane.
- **`cld config`** — print the effective configuration as YAML (defaults merged
  with your `cld.yaml`). Use it to check what settings are in effect.
- **`cld version`** — print the cld version and build info.

### Internal

You won't run these by hand; the daemon and the attach clients do:

- **`cld agent export`** — serves your host ssh-agent on the cld socket so the
  daemon can relay it into sessions; `cld it`/`cld up` start it automatically.
- **`cld x …`** (`exec`, `watch`, `agent`, `api`) — in-container / tmux-pane
  helpers the daemon drives over `docker exec`: running claude in a pane,
  watching files for backup, and relaying the ssh-agent and the control API into
  the container (the last is what makes in-container `cld it` work).

A global `--config <path>` overrides which `cld.yaml` is loaded.

### Shell completion

`cld` completes subcommands and flags, and — for `cld it` / `cld down` /
`cld purge` — the live devcontainer names (and aliases) the daemon is tracking.
Enable it in zsh:

```sh
$ source <(cld completion zsh)   # or add this line to ~/.zshrc
```

## Configuration

All settings are optional; see `cld.yaml` for the full list with defaults.

By default each container logs in for itself and cld persists that login to the
project's isolated backup, restoring it on every recreate — so you log in once
per project, no `~/.claude` bind mount required, and because the backup is
per-project one container's rotating token never clobbers another's.

To instead share ONE Claude-subscription login across sessions, run `cld auth
login` — the daemon takes ownership of the login and refreshes it centrally —
then opt the projects that should use it in with `cld up --proxy` (or `cld it
--proxy`). Those sessions authenticate through the daemon's proxy: no refresh
token ever enters a container, and a running session picks up rotated tokens
without restarting. Your host's own `~/.claude` is left untouched. The proxy is
opt-in because it points `ANTHROPIC_BASE_URL` at a non-first-party endpoint,
which makes Claude Code degrade its UI and disable some features; `--proxy` /
`--no-proxy` are remembered per project. See `docs/claude-config-layout.md` for
how the config tiers relate.

cld's own user-default Claude Code config (settings, `CLAUDE.md`, commands,
agents, output styles — see "Your claude config comes with you" above) is
propagated into every session by default; see `auth.share_config` in
`cld.yaml` to disable it.

See `plan.md` for the design and roadmap.

## Development

The dev container ships a Docker-in-Docker sidecar; integration tests run
against it via `DOCKER_HOST`.

```sh
$ go test ./...            # unit + integration (DinD)
$ go test -short ./...     # unit only
$ CLD_E2E_REAL=1 go test ./internal/daemon/ -run TestRealClaudeInstall  # real download
```
