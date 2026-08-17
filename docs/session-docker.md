# A Docker engine for the session

cld gives claude sessions a Docker engine to work with: a docker-in-docker
container on a private network, with `DOCKER_HOST` pointed at it. Nothing in the
project's `devcontainer.json` changes, and it works for a container VS Code
started as well as one `cld up` did.

**One engine is shared by every project**, so there is one BuildKit cache that
every build warms and one daemon's worth of memory, rather than a copy per
project.

It is **on by default**. To turn it off — globally, or for one project:

```yaml
# cld.yaml
docker:
  mode: off
```

## Read this before leaving it on

**Running an engine is a risk you are choosing to take.**

- Giving a session an engine gives it **root on that engine**. claude, and any
  code claude runs, can start containers, mount your workspace into them, and
  run privileged containers of its own.
- The engine container runs **`--privileged` and shares the host kernel**. It is
  not a VM. Container escapes are a real class of bug, and cld adds no sandbox
  of its own around it.
- The engine listens **without TLS** on the private network. Anything on that
  network — cld's own devcontainers, by design — has full control of it.
- With the shared engine, **projects are not isolated from each other's
  containers, images, or build cache.** Any session can inspect, stop, or delete
  what another project is running there. Use `scope: project` where that
  matters.

This is meaningfully safer than mounting the host's Docker socket into the
container, which hands over the host engine and with it the host. It is not
"safe" — decide deliberately, and turn it off for machines or projects where
that trade does not hold.

## Configuring it

```yaml
docker:
  mode: dind          # dind (default) | off
  scope: shared       # shared (default) | project
  image: docker:dind  # optional
```

Every field works at the top level (all managed containers) and inside a
`projects` block (matching workspaces only). A project block overrides field by
field, so it can change one thing without restating the others — including
`mode: off` to exclude a single workspace.

Pin the image (`docker:28-dind`) if you would rather decide when the engine
version changes. `docker:dind-rootless` trades capability for a smaller blast
radius; expect storage-driver and networking limitations in return.

The `docker` CLI itself is not installed by cld — that is a devcontainer
feature's job (`ghcr.io/devcontainers/features/docker-outside-of-docker`, whose
socket mounting you simply do not use). cld only points it at an engine.

## Shared or per-project

| | `scope: shared` (default) | `scope: project` |
| --- | --- | --- |
| engines | one, for every project | one per project |
| build cache | one, warmed by all of them | separate per project |
| memory | one daemon | one daemon per project |
| `docker build` | works | works |
| `docker run -v <workspace>` | **does not resolve** | works |
| projects isolated from each other | no | yes |

The workspace bind is the whole difference, and it is not an oversight: mounts
are fixed when a container is created, so a shared engine would have to be
recreated — killing everything the other projects were running in it — each time
a new project appeared. Builds do not need it, because a build context is
streamed by the client rather than read off the engine's filesystem. Set
`scope: project` for the projects whose tests bind the source tree
(testcontainers, a compose file with `.:/app`).

## How it works

```
 ┌────────────────┐   private bridge network   ┌──────────────────────┐
 │ devcontainer A │──┐                      ┌─▶│ cld-dind             │
 └────────────────┘  │   tcp://cld-dind:2375│  │ (privileged engine)  │
 ┌────────────────┐  ├──────────────────────┘  │ /var/lib/docker → vol│
 │ devcontainer B │──┘                         └──────────────────────┘
 └────────────────┘
```

cld cannot add a mount to a container that is already running — mounts are
fixed when a container is created — but it **can attach a network to one**.
That is the whole reason this works for containers cld did not create.

During provisioning, before the session starts, cld:

1. creates the engine's network if it does not exist,
2. creates and starts the engine container if it is not there (replacing one
   whose spec no longer matches),
3. attaches the devcontainer to the network,
4. waits for the engine to answer, then
5. adds `DOCKER_HOST` to the session environment.

Every step is best-effort. If the engine cannot be brought up, the session comes
up **without** `DOCKER_HOST` rather than pointed at an engine that is not there
— check `cld setting env <name>` and the daemon log.

The first use on a host pulls the engine image, which takes a while — that is
the one visible cost of the default, and it happens once per host, not per
project.

## `DOCKER_HOST` is a default, not a promise

cld's engine sits **below** your own configuration in the environment layering,
so this still wins:

```yaml
projects:
  - match: ~/work/infra/**
    env:
      DOCKER_HOST: tcp://build01.internal:2376   # use a real engine instead
```

`cld setting env <name>` shows which one took, and where it came from — `cld
docker` for cld's engine, `cld.yaml …` for yours.

## Adding a volume to the engine

The common case, start to finish. Say you want a host directory available to
the engine — a shared build cache, a certificate bundle, a dataset your builds
read.

**1.** Open the override — `cld config edit dind` creates it next to your
`cld.yaml` (that is `~/.config/cld/cld.dind.yaml`) with a commented template,
and refuses to save something cld could not act on:

```yaml
services:
  dind:
    volumes:
      - /srv/build-cache:/cache
```

Writing the file by hand does the same thing; the editor just catches an
unknown key or a relative source now instead of at the next provisioning.

The source is a **host path**, because the engine resolves it on the host. Write
it absolute, or against your home — `~/workspaces:/workspaces` and
`${HOME}/workspaces:/workspaces` both work, since cld knows the host's own home
and expands them before creating the engine. Anything else relative, and a named
volume, is rejected: they cannot be resolved there. The target is a path inside
the engine container, and is never expanded.

Anything you add is *added to* what cld already mounts; the engine keeps its own
storage.

**2.** Restart the daemon, which reads the config at startup:

```sh
$ docker restart cld
```

**3.** The engine is replaced on the next provisioning, because its spec
changed — a container cannot be given a mount in place. With the shared engine
that takes every project's running containers with it, so do this when nothing
is mid-build. The image and build cache survive (they live in a volume).

**4.** Check it:

```sh
$ cld docker -- run --rm -v /cache:/c alpine ls /c
```

That is also how the volume becomes useful: `/cache` now exists **in the
engine**, so a `-v /cache:/...` from a session resolves to your host directory —
the same indirection the workspace bind uses under `scope: project`.

A few things to know:

- With `scope: shared` (the default) the volume is on the one engine, so every
  project gets it. Only a project with `scope: project` can have its own.
- Read-only is `- /etc/ssl/corp:/etc/docker/certs.d:ro`.
- A named volume cannot be created this way; give a path.
- `~` expands to your **host** home, not the daemon's or the container's. If cld
  cannot determine it — a daemon installed without the home mount — it says so
  and leaves the engine alone rather than creating a directory named `~` on your
  host.

## Overriding the engine container

The volume above is one key of a larger override. `cld config edit dind` opens
it — a **`cld.dind.yaml`** next to your `cld.yaml` — and cld folds it into the
engine it creates:

```yaml
# ~/.config/cld/cld.dind.yaml
services:
  dind:
    command: ["--insecure-registry", "registry.internal:5000"]
    volumes:
      - /etc/ssl/corp:/etc/docker/certs.d:ro   # HOST paths (~/ expands)
    environment:
      HTTP_PROXY: http://proxy.internal:3128
    mem_limit: 8g
    cpus: 4
```

Point at a different file with `docker.compose` — per project, if you like:

```yaml
projects:
  - match: ~/work/infra/**
    docker: {scope: project, compose: infra.dind.yaml}
```

A relative name resolves against the config directory. A file named explicitly
must exist (a typo is an error, not a silently skipped override); the default
`cld.dind.yaml` is used only if it is there.

An override on the **shared** engine applies to it for everyone — the first
project to bring it up decides, and a project that resolves a different override
would rebuild the engine out from under the others. Scope an override to one
project only together with `scope: project`.

**It is compose-shaped, not compose.** cld builds the engine through the Docker
API and runs no compose CLI, so the supported keys are the ones that map onto a
container:

| | |
| --- | --- |
| replace what cld set | `image`, `command`, `entrypoint`, `privileged`, `mem_limit`, `cpus` |
| merge key by key | `environment`, `labels`, `sysctls` |
| append to what cld set | `volumes`, `cap_add`, `cap_drop`, `devices`, `security_opt`, `extra_hosts`, `dns`, `ports` |

Anything else — `healthcheck`, `depends_on`, `networks`, … — is an **error when
the file is read**. Accepting a key and quietly ignoring it would leave you
believing it applies.

Two rules worth remembering:

- **Volume sources must be absolute host paths.** The engine resolves them on
  the host, where the daemon's own view of the filesystem does not apply, so
  `~` cannot be expanded and a named volume cannot be created.
- **Editing the override rebuilds the engine.** A container cannot be
  reconfigured in place; cld hashes the resolved spec into a label and replaces
  the engine when it changes. The image cache volume survives.

Running rootless is `privileged: false` plus whatever your image needs:

```yaml
services:
  dind:
    image: docker:dind-rootless
    privileged: false
```

## Talking to the engine: `cld docker`

The engine sits on a private network with its devcontainer and is not reachable
from your host, so `docker -H …` will not find it. `cld docker` runs the command
inside the engine's own container, where the CLI already lives:

```sh
$ cld docker                          # what and where the engine is
cld-api-c24c3b-dind  tcp://cld-api-c24c3b-dind:2375  running

$ cld docker -- ps
$ cld docker -- images
$ cld docker -- run --rm -it alpine sh    # interactive, as you would expect
$ cld docker --name api -- system prune -f
```

Everything after `--` goes to `docker` untouched, which is what keeps its flags
(`--rm`, `-it`) from being read as cld's own. With no `--name` it targets the
only devcontainer, like `cld it`. It needs
access to your host's own Docker (the same access `cld up` needs), so it is a
host-side command; inside a devcontainer the plain `docker` CLI already points
at this engine through `DOCKER_HOST`.

## Volume paths: the thing that surprises everyone

A path in `docker run -v` is resolved by the **engine**, not by the container
that typed the command. Inside a docker-in-docker setup that means paths name
the engine container's filesystem, not the devcontainer's — so a bind of your
source tree gives you an empty directory where you expected your files.

Builds are unaffected: `docker build .` streams the context from the client, so
it reads the devcontainer's files and works on any engine.

With `scope: project`, cld binds your host workspace into the engine at **the
same path the devcontainer uses**, so this works as written:

```sh
$ pwd
/workspace
$ docker run --rm -v /workspace:/app alpine ls /app   # your files
```

With the shared engine it cannot, and there is no way around it short of an
engine per project. If a project's tests bind the source tree, give it
`scope: project`.

## Resources and cleanup

Names are derived from a key — `cld` for the shared engine, the project's own
key otherwise — and everything is labelled `cld.dind=<key>`:

| Resource | Shared | Per project |
| --- | --- | --- |
| engine container | `cld-dind` | `<key>-dind` |
| network | `cld-dind-net` | `<key>-dind-net` |
| image + build cache | `cld-dind-data` | `<key>-dind-data` |

| Event | Shared engine | Project engine |
| --- | --- | --- |
| `cld down <name>` / `cld purge <name>` / `docker rm` | **kept** (others use it) | removed |
| `cld down --all` | removed, cache kept | removed, cache kept |
| `cld purge --all` | removed with its cache | removed with its cache |

A cache is kept on a plain teardown so the next `cld up` does not re-pull and
re-build everything; a purge deletes it.

The engine carries no devcontainer labels, so `cld ls` never shows it and cld
never tries to provision it as a devcontainer.

## Limits

- **No filtering of what the session does with the engine.** The devcontainer
  talks to it directly; cld is not in that path and cannot refuse a privileged
  `docker run`.
- **The shared engine gives projects no isolation from each other**, and a
  per-project engine gives them no shared cache. There is no arrangement that
  provides both.
- **The engine is not reachable from your host**, only from the devcontainer on
  its private network — `cld docker` exists because of this. Publish it with
  `ports:` in the override if you really want it exposed, knowing it has no TLS
  and no auth.
- Setting up `docker compose` inside the session works, with the same path rule
  as above for any bind mount it declares.
