# cld architecture

`cld` runs Claude Code inside devcontainers, keeps each one's conversation state
safe, and lets you attach to a live session from your terminal — including from
*inside* a managed devcontainer, not only from the host. This document describes
how the pieces fit together and how in-container access works.

## Components

| Piece | Where it runs | Role |
|-------|---------------|------|
| `cld serve` (daemon) | host, or a compose container with the docker socket | watches docker events, provisions devcontainers, owns the tmux sessions, syncs state |
| tmux server | co-located with the daemon (`<CacheDir>/tmux.sock`) | one session per devcontainer; each pane runs `cld x exec … claude` |
| `cld x exec` | tmux pane (daemon side) | `docker exec`s into the target container and runs `claude` with a TTY |
| `claude` | inside each devcontainer | the Claude Code process, installed by the daemon at `/usr/local/bin/claude` |
| `cld x watch` / `cld x agent` / `cld x api` | inside each devcontainer | in-container helpers the daemon drives over `docker exec` (file watch, ssh-agent relay, API relay) |
| `cld it` / `cld up` / `cld ls` / `cld down` | wherever you invoke them | control-plane clients that talk to the daemon over `<CacheDir>/cld.sock` |
| `cld install` / `cld uninstall` | host | create/remove the daemon container on the host's Docker; `internal/installer` mirrors the reference `docker-compose.yaml` (drift-tested) |

Key paths (`CacheDir` defaults to `$XDG_CACHE_HOME/cld`, i.e. `~/.cache/cld`;
`DataDir` defaults to `$XDG_DATA_HOME/cld`, i.e. `~/.local/share/cld`):

- `<CacheDir>/cld.sock` — daemon control API (HTTP over a unix socket)
- `<CacheDir>/tmux.sock` — the dedicated tmux server
- `<CacheDir>/agent.sock`, `<CacheDir>/gitconfig` — host shares staged for the
  daemon (ssh-agent, gitconfig)
- `<DataDir>/user-default/` — cld's own user-default Claude Code config (see
  `docs/claude-config-layout.md`); not staged from the host, edited directly
- `<DataDir>/broker-credentials.json`, `<DataDir>/proxy/<key>`,
  `<DataDir>/projects/<key>/` — the opt-in broker login, per-project proxy-mode
  preferences, and each project's isolated backup (which also holds that
  project's persisted `.credentials.json`)

## Topology

The daemon is the only component with a docker client. It manages *sibling*
devcontainers; it never runs inside the container it is provisioning. It is
normally run as a container by `cld install` (equivalently the reference
`docker-compose.yaml`), which is also what enables in-container access below; it
can still run directly on the host via `cld serve`.

```
        ┌────────────────────── daemon side (host or compose container) ──────────────────────┐
        │                                                                                       │
        │   cld serve ──┬── docker events / API (moby client)                                   │
        │               │                                                                       │
        │               ├── tmux server  (<CacheDir>/tmux.sock)                                 │
        │               │      └── session "cld-<name>"                                         │
        │               │             └── pane: cld x exec <ctr> -- claude [--continue|| …]     │
        │               │                                  │  docker exec (TTY)                 │
        │   cld.sock ◄── control API (HTTP)                │                                    │
        └──────────────────────────────────────────────── │ ───────────────────────────────────┘
                                                           ▼
        ┌────────────────────── target devcontainer ──────────────────────┐
        │   claude  (/usr/local/bin/claude, CLAUDE_CONFIG_DIR=~/.cld/claude)│
        │   cld x watch / cld x agent / cld x api   (driven by the daemon)  │
        └──────────────────────────────────────────────────────────────────┘
```

## Provisioning lifecycle (`internal/daemon/ensure.go`)

`ensure` is idempotent and re-runs on every docker event / reconcile. For a
running, non-ignored devcontainer it, in order:

1. **resolve identity** — probe uid/gid/home/cache-dir/libc, compute the config
   dir (`~/.cld/claude`), workspace folder, and platform.
2. **install binaries** — copy `claude-<version>` and the `cld` binary into
   `/usr/local/bin` (atomic symlink swap; checksum-verified).
3. **prepare state** — restore the project backup (including that project's
   persisted `.credentials.json`), install the host gitconfig and your shared
   Claude Code config (settings/memory/commands/agents/output-styles), seed
   onboarding/trust keys, and finally `chown -R` the
   config dir to the container user (docker cp leaves synthesized parent dirs
   root-owned, which would block a new conversation's transcript write).
4. **create the session** — a host tmux session whose pane runs
   `cld x exec … -- claude` (see below).
5. **start watchers** — file-change sync, container watch, and the ssh-agent and
   API relays.

### How `claude` is launched

`ensure_session` builds the pane command and hands it to tmux. The remote argv
(`session_command`) resumes a prior conversation but never lets a failed resume
kill the session:

```
claude                                   # no prior transcript
sh -c 'claude --continue || exec claude' # prior transcript: resume, else fresh
```

`cld` cannot perfectly predict Claude Code's private `projects/<encoded-cwd>`
directory naming (it truncates and hashes long paths and normalizes unicode).
`internal/claude.EncodeProjectPath` mirrors that encoding for backup/restore
placement, but session *liveness* does not depend on it: a resume Claude Code
rejects degrades to a fresh session instead of exiting with `no conversation
found to continue`.

A pane's exit status is reported back to the daemon (`cld x exec` →
`POST /notify/exited?code=…`). A clean exit (0) becomes `session-ended`; a
non-zero exit becomes `failed` and stays visible, so a crash is diagnosable
rather than looking like a normal quit.

### Extra panes open a container shell

The tmux server runs on the *host*, so a plain `prefix-%` would split into a
host shell — one level out from where you want to be. `cld` rebinds the split
and new-window keys so an extra pane instead drops you straight into a shell
inside that session's container:

| key            | opens                                       |
| -------------- | ------------------------------------------- |
| `prefix + %`   | container shell, split right                |
| `prefix + "`   | container shell, split down                 |
| `prefix + c`   | container shell in a new window             |

The shell is the same `cld x exec` attach the claude pane uses, pointed at a
login shell (`${SHELL:-bash}`, falling back to `sh`) instead of `claude`, and
carrying the same session environment (`session_env`). It omits
`--notify`/`--session-gen`, so closing an ad-hoc shell never ends the claude
session.

One tmux server is shared by every session, but `bind-key` is server-global and
has no per-session scope — and tmux does **not** expand `#{format}` strings in a
split-window command argument. So the bindings are identical for all sessions
and defer the container to a session environment variable, `CLD_EXEC`, which
`ensure_session` sets per session (`SetSplitCommand`) to that session's own
`cld x exec … -- sh -c 'exec ${SHELL:-bash} …'`. A new pane inherits its
session's environment, so `sh -c "$CLD_EXEC"` resolves to the right container at
key-press time (`internal/tmuxx.bindSplitKeys`).

## Attaching from your terminal

`cld it <name>` asks the daemon where it lives (`GET /info`) and attaches by the
route that fits the deployment:

- **docker-exec attach** (`attach_via_exec`) — the daemon runs in a container
  this host can see: `docker exec <daemon-ctr> tmux attach`. The host needs no
  tmux.
- **local attach** (`attach_local`) — the daemon runs on this host:
  `tmux -S <tmux.sock> attach`.
- **API attach** (`AttachSession`) — the client cannot reach the daemon's docker
  or tmux directly, e.g. it is running *inside* a managed container. See
  [in-container access](#in-container-access).

## State sync & relays

- **Backups** (`internal/syncer`) — `projects/<enc>` transcripts and a
  settings snapshot are copied out on change into that container's own
  isolated per-project backup dir (never a bucket shared across projects —
  see `docs/claude-config-layout.md`) and restored into a fresh container of
  the *same* project, rewriting the workspace path if it moved. Credentials
  are never backed up.
- **ssh-agent relay** (`internal/agentx`, `relay_agent`) — the daemon runs
  `cld x agent <sock>` in the container and bridges that socket, over the
  `docker exec` stdio, to the host ssh-agent — so `git push`/commit signing over
  SSH work inside the container. `agentx` is a *generic* multiplexing relay: a
  container-side unix socket ⇄ a duplex byte stream ⇄ a freshly-dialed socket on
  the daemon side. Only the daemon-side dial target is agent-specific.

  Because it relays SSH, in-container git should use an **SSH remote**
  (`git@github.com:…`). A host `credential.helper` (e.g. `gopass`, `osxkeychain`)
  is a host-only binary, so `cld` strips it from the forwarded gitconfig
  (`install_gitconfig`) rather than shipping a helper the container cannot run;
  HTTPS remotes therefore rely on whatever the container itself provides.
- **Config sharing** (`install_claude_config`) — the daemon *mirrors*
  `<DataDir>/user-default/` (settings.json, CLAUDE.md, and the
  commands/agents/output-styles directories; never credentials, `.claude.json`,
  or history) into each session's config dir on every provision — installing
  what is present, removing what user-default dropped. This is a directory
  cld owns, not your host's `~/.claude` — cld never reads or writes that;
  you populate user-default directly. Since `CLAUDE_CONFIG_DIR` is that dir,
  claude reads them as user-level config. `settings.json` is the base cld's
  own keys merge onto, after `SanitizeUserSettings` drops secret/host-only
  keys (`env`, the apiKeyHelper/aws*/otel helpers, project-MCP auto-trust) —
  and skips it entirely if it is not a JSON object, so a malformed file can
  never fail the seed. Off with `auth.share_config: false`.

The same generic relay carries the control API into containers.

## In-container access

The daemon's control API (`cld.sock`) and tmux server live on the daemon side
and are never mounted into a managed container, so `cld it`/`cld ls` run *inside*
a container have nothing to reach on their own: `~/.cache/cld` is absent and a
devcontainer normally has no route back to the daemon. The daemon only reaches
*into* containers over `docker exec`; the control API and a pty are relayed back
out over that same channel.

(Relaying the tmux socket itself would not work: tmux's client/server protocol
passes file descriptors over the socket via `SCM_RIGHTS`, which a byte relay
cannot carry — so the daemon streams a pty rather than proxying tmux.)

### Reaching the daemon

The control API is exposed inside each container with the same relay as the
ssh-agent, pointed at a different daemon-side target:

- **container side** — `cld x api <sock>` runs `agentx.ListenAndServe` on
  `<cache>/cld/cld.sock`, the path an in-container `cld` dials for the daemon by
  default, so it needs no configuration.
- **daemon side** — `relay_api` bridges each connection to a **per-container,
  self-scoped** API served in-process (`scoped_api`), not to the full
  `cld.sock`. `relay_agent` and `relay_api` share one parameterized
  `relay`/`relay_once`, differing only in the container-side socket and the
  daemon-side dial.

```
  in-container cld ──▶ <cache>/cld/cld.sock             (cld x api: ListenAndServe)
                              │ multiplex over docker-exec stdio (agentx)
                              ▼
        daemon: agentx.Bridge(dial = pipeListener) ──▶ scoped_api(container-id)
                                                          (in-process http.Server
                                                           over net.Pipe)
```

The bridge dials an in-memory `pipeListener` whose `http.Server` runs
`scoped_api(id)` with the owning container's id baked in, so requests can only
act on that container's own session (see [security](#constraints--security)).
`cld ls`, `cld it --new` (RecreateSession), and `cld down` on the container
itself are then plain HTTP calls over the relayed socket.

### Attaching over the control socket

Attaching needs a terminal, which the control HTTP API does not carry directly.
A dedicated endpoint streams a pty:

- **`GET /session/attach?name=&cols=&rows=&term=`** (`handle_attach`) hijacks the
  connection and runs `tmux attach-session` for `cld-<name>` **inside the
  daemon's own container** (`docker exec` supplies the TTY and resize, via
  `termx.Stream`), piping it to the hijacked connection. It is offered only when
  the daemon runs in a container (`Info.APIAttach = self_ctr != ""`); a host-run
  daemon leaves in-container attach to the host.

The hijacked stream rides the same `agentx` relay, so the client needs neither
docker nor tmux — just the one unix socket.

#### Wire protocol (after the HTTP `101` upgrade)

One connection carries keystrokes *and* resizes, so the client→daemon direction
is framed while daemon→client is the raw pty:

```
client → daemon:
  'd' u32:len  <len bytes>     # stdin data
  'w' u16:cols u16:rows        # window resize
daemon → client:
  <raw pty bytes>              # terminal output
```

`read_attach_frames` decodes the framed side; resizes feed the `termx.Stream`
size channel (`ExecResize`); oversized/garbage frames are rejected. The exec's
lifetime is tied to the connection, so a dropped client does not orphan a tmux
attach on the daemon side.

### How `cld it` chooses a route

```
GET /info, then:
  daemon in a container this host can see   → docker-exec attach   (host)
  else the daemon offers API attach         → API attach           (in-container)
  else (daemon on this host)                → local tmux attach
```

`daemon_container_reachable` tells host from in-container by inspecting the
daemon's container id through the local docker client: on the host that
succeeds; inside a managed container it does not, which routes to API attach.

### VS Code terminal profile

A bare `cld it` inside a container attaches to that container's own session, so
it works as a VS Code integrated-terminal profile: pick **claude** from the
terminal `+` dropdown to drop straight into the session.

cld seeds this profile automatically into the container's editor machine
settings during provisioning (`~/.vscode-server` and `~/.cursor-server`,
`data/Machine/settings.json`), best-effort — an unused editor just gets an
unused settings file. For any other editor, or to control it yourself, add it to
your `devcontainer.json`:

```jsonc
"customizations": {
  "vscode": {
    "settings": {
      "terminal.integrated.profiles.linux": {
        "claude": { "path": "cld", "args": ["it"] }
      }
      // make it the default terminal (optional):
      // "terminal.integrated.defaultProfile.linux": "claude"
    }
  }
}
```

The profile relies on the API relay, so it attaches when in-container `cld it`
can — i.e. with a containerized daemon.

### Constraints & security

- **Self-scoped, no lateral movement.** The relayed API is `scoped_api(id)`, not
  the full control plane. `/items` returns only the caller's own container;
  `/session/attach`, `/session/new`, `/down`, and `/notify/exited` reject any
  request whose target is not the caller's own container. The identity is bound
  on the daemon side (the container id the relay serves), never supplied by the
  container, so untrusted code in one project cannot enumerate, attach to,
  recreate, or destroy another project's session. This matters because managed
  containers run agent-driven, potentially untrusted repository content. The
  fleet-wide `/down/all` (behind `cld down --all`) is deliberately absent from
  the scoped API — only the host-side control plane exposes it — so a managed
  container cannot tear the whole fleet down. The `/purge` and `/purge/all`
  endpoints (behind `cld purge`) are likewise host-only: a managed container
  cannot delete any project's volumes or conversation backup, not even its own.
- **Opt-out.** `auth.remote_control: false` disables the relay entirely
  (`RemoteControlEnabled`), symmetric to `forward_agent` for the ssh-agent
  relay. It is on by default.
- **No network surface.** The API is reachable in a container only through the
  daemon-initiated `docker exec` relay — only inside containers the daemon
  already provisions. There is no TCP listener and no token to leak.
- **Socket location.** The relay socket path matches `os.UserCacheDir`
  (`$XDG_CACHE_HOME` or `$HOME/.cache`, probed per container in `resolve`), so an
  in-container `cld` finds it with no config. (An `XDG_CACHE_HOME` set only in an
  interactive shell rc — not the container environment — could still diverge.)
- **Deployment.** API attach requires a containerized daemon (`self_ctr != ""`);
  a host-run daemon advertises `APIAttach = false` and in-container attach falls
  through. The control commands (`ls`/`new`/`down`) work against either
  deployment.
- **Same-arch only.** The relays (like the watcher) run when the container arch
  matches the host, which is also when the in-container `cld` binary can run.

### Test coverage

- `internal/daemon/attach_test.go` — the attach frame codec (round-trip,
  oversized, bad type, non-blocking resize).
- `internal/daemon/scoped_api_test.go` — the scoping: a container reaches only
  its own session and is refused any other name/container.
- `internal/daemon/gitconfig_test.go` — the host `credential.helper` is stripped
  from the forwarded gitconfig, identity kept.
- `internal/daemon/vscode_test.go` — the terminal-profile merge preserves other
  settings/profiles and is idempotent.
- `internal/daemon/integration_test.go` — an in-container `cld ls` reaches the
  daemon through the relay and lists the container.
