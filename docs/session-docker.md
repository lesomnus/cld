# A Docker engine for the session

cld can give a project's claude session a Docker engine of its own: a
docker-in-docker container on a private network, with `DOCKER_HOST` pointed at
it. Nothing in the project's `devcontainer.json` changes, and it works for a
container VS Code started as well as one `cld up` did.

It is off by default.

```yaml
# cld.yaml
projects:
  - match: ~/work/infra/**
    docker:
      mode: dind
```

## Read this before enabling it

**Turning this on is a risk you are choosing to take.**

- Giving a session an engine gives it **root on that engine**. claude, and any
  code claude runs, can start containers, mount your workspace into them, and
  run privileged containers of its own.
- The engine container runs **`--privileged` and shares the host kernel**. It is
  not a VM. Container escapes are a real class of bug, and cld adds no sandbox
  of its own around it.
- The engine listens **without TLS** on the private network. Anything on that
  network — which is the devcontainer, by design — has full control of it.
- Your workspace is bind-mounted into the engine container, so control of the
  engine means access to those files.

This is meaningfully safer than mounting the host's Docker socket into the
container, which hands over the host engine and with it the host. It is not
"safe". That is why the default is `off` and why there is no way to turn it on
for every project at once without saying so explicitly.

## Enabling it

```yaml
docker:
  mode: dind          # off (default) | dind
  image: docker:dind  # optional
```

Both fields work at the top level (every container cld manages) and inside a
`projects` block (only matching workspaces). A project block overrides field by
field, so it can change the image without restating the mode — or set
`mode: off` to exclude one workspace from a global setting.

Pin the image (`docker:28-dind`) if you would rather decide when the engine
version changes. `docker:dind-rootless` trades capability for a smaller blast
radius; expect storage-driver and networking limitations in return.

The `docker` CLI itself is not installed by cld — that is a devcontainer
feature's job (`ghcr.io/devcontainers/features/docker-outside-of-docker`, whose
socket mounting you simply do not use). cld only points it at an engine.

## How it works

```
 ┌────────────────┐   private bridge network   ┌──────────────────────┐
 │ devcontainer   │───────────────────────────▶│ <project>-dind       │
 │ DOCKER_HOST=   │      tcp://…:2375          │ (privileged engine)  │
 │  tcp://…-dind  │                            │ /var/lib/docker → vol│
 └────────────────┘                            └──────────────────────┘
```

cld cannot add a mount to a container that is already running — mounts are
fixed when a container is created — but it **can attach a network to one**.
That is the whole reason this works for containers cld did not create.

During provisioning, before the session starts, cld:

1. creates the project's network if it does not exist,
2. creates and starts the engine container if it is not there (replacing one
   whose image or workspace no longer matches),
3. attaches the devcontainer to the network,
4. waits for the engine to answer, then
5. adds `DOCKER_HOST` to the session environment.

Every step is best-effort. If the engine cannot be brought up, the session comes
up **without** `DOCKER_HOST` rather than pointed at an engine that is not there
— check `cld setting env <name>` and the daemon log.

The first use on a host pulls the engine image, which takes a while.

## `DOCKER_HOST` is a default, not a promise

cld's engine sits **below** your own configuration in the environment layering,
so this still wins:

```yaml
projects:
  - match: ~/work/infra/**
    docker: {mode: dind}
    env:
      DOCKER_HOST: tcp://build01.internal:2376   # use the real engine instead
```

`cld setting env <name>` shows which one took, and where it came from — `cld
docker` for cld's engine, `cld.yaml …` for yours.

## Volume paths: the thing that surprises everyone

A path in `docker run -v` is resolved by the **engine**, not by the container
that typed the command. Inside a docker-in-docker setup that means paths refer
to the engine container's filesystem, not the devcontainer's — so a naive setup
gives you an empty directory where you expected your source.

cld avoids this by bind-mounting your host workspace into the engine at **the
same path the devcontainer uses**. So this works as written:

```sh
$ pwd
/workspace
$ docker run --rm -v /workspace:/app alpine ls /app   # your files
```

Only the workspace is shared this way. Any other host path you bind will resolve
inside the engine container and almost certainly not be what you meant.

## Resources and cleanup

Everything is derived from the project key, and labelled `cld.dind=<key>`:

| Resource | Name | `cld down` / container removed | `cld purge` |
| --- | --- | --- | --- |
| engine container | `<key>-dind` | removed | removed |
| network | `<key>-dind-net` | removed | removed |
| image cache | `<key>-dind-data` (volume) | **kept** | removed |

The image cache is kept on a plain teardown so the next `cld up` does not re-pull
everything; `cld purge` deletes it along with the rest of the project's state.

A devcontainer removed outside cld (`docker rm`) also takes its engine with it —
the daemon sees the destroy event and cleans up. The engine carries no
devcontainer labels, so `cld ls` never shows it and cld never tries to provision
it as a devcontainer.

One engine per project. Two projects never share an engine, an image cache, or a
network.

## Limits

- **No filtering of what the session does with the engine.** The devcontainer
  talks to it directly; cld is not in that path and cannot refuse a privileged
  `docker run`.
- **No shared engine across projects.** Isolation is the point; the cost is a
  separate image cache per project.
- **The engine is not reachable from your host**, only from the devcontainer on
  its private network.
- Setting up `docker compose` inside the session works, with the same path rule
  as above for any bind mount it declares.
