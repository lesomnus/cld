# Session environment, files, and scripts

cld lets you declare what a claude session runs with — environment variables,
host files placed in the container, and scripts run before the session starts —
in your own `cld.yaml`, globally or per project. Nothing here touches the
project's `devcontainer.json` or its image.

The motivating case is handing claude something the container does not have and
the repo should not carry: a remote Docker endpoint and its credentials, a cloud
profile, a token. But the same three sections cover everyday personalization.

```yaml
env:
  EDITOR: vim

projects:
  - match: ~/work/acme/**
    env:
      DOCKER_HOST: tcp://build01.internal:2376
      DOCKER_TLS_VERIFY: "1"
      DOCKER_CERT_PATH: ${HOME}/.docker-remote
    files:
      - src: ~/.docker/build01/
        dst: ${HOME}/.docker-remote
```

## What this applies to (and what it does not)

cld does not create your devcontainer — the devcontainer CLI or VS Code does,
and the daemon provisions the container it finds. So everything here applies to
**the processes cld starts**:

| Reached by cld's environment                  | Not reached                            |
| --------------------------------------------- | -------------------------------------- |
| the claude session (`cld it`, `cld up`)        | other VS Code terminals                |
| panes you split off it (`ctrl-b %`, `"`, `c`) | `postCreateCommand` and friends        |
| the `scripts` below                            | a `docker exec` you run yourself       |
| the **claude** VS Code terminal profile        |                                        |

For credentials this is a feature, not a limitation: what cld injects never
enters the container's own configuration, so it does not show up in
`docker inspect` and is not inherited by everything else running in there.

If you need a variable for the *whole* container, put it in `devcontainer.json`
(`containerEnv`), which is also the right place for ports and mounts. cld does
not compete with it — and it now **applies your `remoteEnv`** as well, so a
variable you already declared there reaches claude too.

## Scoping: `projects`

The top-level `env`, `files`, and `scripts` apply to every container cld
manages. A `projects` entry scopes the same three to the workspaces its globs
match:

```yaml
projects:
  - match: ~/work/**            # a string or a list of them
    env: {NPM_TOKEN: "${env:NPM_TOKEN}"}
  - match: [~/work/acme/**, ~/contrib/acme-*]
    env: {AWS_PROFILE: acme}
```

Globs are matched against the **host-side** workspace path, with the same rules
as the top-level `ignore` list: `**` crosses path separators and a leading `~/`
expands to your home directory.

Every matching block applies, in file order — the first match does not win, so a
broad block and a narrow one compose. Later blocks override earlier ones on a
key-by-key basis.

## `env`

### Where a value comes from

Variables are resolved in layers. Later layers win:

1. the container's own environment (its image `ENV` and `containerEnv`)
2. the devcontainer's `remoteEnv`
3. cld's defaults — `TERM`, `LANG`, `DISABLE_AUTOUPDATER`
4. `env` in your `cld.yaml`
5. `projects[*].env`, for every matching block, in file order
6. the variables cld manages (see below)

So **your `cld.yaml` wins over `devcontainer.json`**: that file is the project's
shared contract, this one is your machine. When you would rather defer to what
is already there, say so with the value syntax.

### Value syntax

```yaml
env:
  FOO: bar                        # set it, whatever was there
  PATH: ${PATH}:/opt/mytools/bin  # extend what was there
  GOFLAGS: ${GOFLAGS:--mod=mod}   # only if it is unset or empty
  LESS: null                      # remove it from the session
  TOKEN: ${env:NPM_TOKEN}         # read the daemon's own environment
  LITERAL: 100$$                  # $$ is a literal "$"
```

- `${NAME}` sees the layers resolved **before** this one, so `${PATH}:/x`
  composes with what the image set — and composes again in a `projects` block.
- Two variables in the same block cannot see each other. That keeps the result
  from depending on YAML map ordering.
- `${env:NAME}` reads the **daemon's** environment. This is how a secret reaches
  a session without being written into `cld.yaml`: put it in the daemon's
  `docker-compose.yaml` (`environment:`) or its `cld install` invocation, and
  reference it here.
- A lone `$` is literal, so a value like `p$ssw0rd` needs no escaping.
- `null` genuinely removes the variable: a `docker exec` cannot drop what the
  container passes down, so cld unsets it in the session command itself.

### Variables cld manages

These are rejected in `cld.yaml` — loading fails with an error rather than
quietly ignoring you:

`CLAUDE_CONFIG_DIR`, `GIT_CONFIG_GLOBAL`, `SSH_AUTH_SOCK`, `ANTHROPIC_BASE_URL`,
`ANTHROPIC_AUTH_TOKEN`, `ENABLE_TOOL_SEARCH`, `CLAUDE_CODE_ENABLE_TELEMETRY`,
and anything starting with `OTEL_`.

cld points these at the config it seeds, the ssh-agent relay, the auth proxy and
the telemetry receiver; a session that quietly lost one would break in a way no
error message explains. They are rejected whether or not the feature that sets
them is currently on, so whether your config loads never depends on daemon
state.

`TERM`, `LANG` and `DISABLE_AUTOUPDATER` are cld's *defaults*, not its promises —
override them freely.

## `files`

For what a variable cannot carry: TLS material, a `config.json`, a key.

```yaml
files:
  - src: ~/.docker/build01/     # host path, always written as "~/..."
    dst: ${HOME}/.docker-remote # container path
    mode: "0600"                # optional; 0600 by default
```

- `src` must be under your home directory and written as `~/...`. The daemon
  reads your host through the same read-only home mount dotfiles uses
  (`/host-home`), so nothing outside it is readable — and an absolute path would
  mean different things to a containerized daemon and a host one.
- `dst` is an absolute container path; `${HOME}` and `${CLD_WORKSPACE}` expand.
- Files land owned by the container user, at `mode` (`0600` by default, since
  credentials are the motivating case). A directory is copied whole, with
  execute added wherever the mode grants read.
- Each placement records a content hash in the container, so an unchanged source
  costs a hash and a rotated credential is re-copied.
- Placement happens while a container is being provisioned — at container start,
  after a daemon restart, on re-provisioning — not on a timer. A credential that
  rotates while a session is up reaches the next one.

## `scripts`

```yaml
scripts:
  setup: sudo apt-get update && sudo apt-get install -y ripgrep
  start: echo "started $CLD_NAME"
```

| Script  | Runs                                   |
| ------- | -------------------------------------- |
| `setup` | once per container                     |
| `start` | once per container start (every boot)   |

Both run **after** everything cld installs (claude, its config, dotfiles, the
placed files) and **before** the session exists, so what a script installs is
there from claude's first prompt.

`setup` re-runs when its own definition changes — edit it and the next
provisioning applies it. (Editing any setup script re-runs all of them for that
container; they are treated as one unit.) A container recreated from scratch
runs it again.

The long form, when the defaults do not fit:

```yaml
scripts:
  setup:
    run: [make, dev-setup]    # a list runs with no shell; a string runs under sh -c
    user: root                # default: the container user claude runs as
    workdir: ${CLD_WORKSPACE} # default: the workspace folder
    timeout: 10m              # default: 5m
    on_error: fail            # default: warn
```

- Scripts see the session environment resolved above, plus `CLD_EVENT`,
  `CLD_NAME`, `CLD_CONTAINER_ID`, `CLD_WORKSPACE`, and `CLD_STARTED_AT`.
- `on_error: warn` (the default) logs the failure and lets provisioning
  continue — a personal script must not be able to lock you out of a session.
  `on_error: fail` marks the container failed instead, and is retried on the
  next provisioning pass rather than being recorded as done.
- The timeout stops cld waiting; it does not kill the process inside the
  container. Its purpose is to keep a hanging script from stalling that
  container's other work.
- A global script and a matching project's both run, global first.

## Checking what took effect

```sh
$ cld setting env acme
AWS_PROFILE   acme                        cld.yaml projects[~/work/acme/**]
PATH          /usr/bin:/opt/mytools/bin   cld.yaml env
LANG          C.UTF-8                     cld default
EDITOR        vim                         devcontainer remoteEnv
LESS          (removed)                   cld.yaml env
CLAUDE_CONFIG_DIR  /home/dev/.claude      cld
```

The third column is the point: when a variable does not take, what you need is
the layer that won. `--export` prints shell assignments instead, for `eval`.

## When changes take effect

The environment is fixed when a session is created, so:

- **an existing session keeps its environment** — restart it with
  `cld it --new <name>`, or restart the container;
- **the daemon reads `cld.yaml` at startup** — restart the daemon
  (`docker restart cld`) after editing it.

## Not supported

- Reading a `cld.yaml` from inside the workspace. Anyone who can write to the
  repo could then inject environment variables and shell scripts into your
  session; that job belongs to `devcontainer.json`, which is reviewed as part of
  the repo.
- `${localEnv:NAME}` in a `devcontainer.json` value: the daemon runs in a
  container and cannot see your shell's environment. It resolves to empty.
- Container-wide environment, ports, and mounts — `devcontainer.json`.
- Installing a docker CLI into the container — that is a devcontainer feature's
  job (`docker-outside-of-docker`); cld only points `DOCKER_HOST` at the engine
  you choose.

## A note on `DOCKER_HOST`

cld already sets `DOCKER_HOST` for its own pane client on the host, so it can
reach the engine your devcontainer runs on. The `DOCKER_HOST` you set here is a
different thing: it is what **claude inside the container** talks to. They are
injected at different layers and can hold different values.
