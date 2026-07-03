# cld architecture

`cld` runs Claude Code inside devcontainers and keeps each one's conversation
state safe, while letting you attach to a live session from your terminal. This
document explains how the pieces fit together, and then details the
**remote-control** feature that lets `cld it` work *from inside* a managed
devcontainer — not just from the host.

## Components

| Piece | Where it runs | Role |
|-------|---------------|------|
| `cld serve` (daemon) | host, or a compose container with the docker socket | watches docker events, provisions devcontainers, owns the tmux sessions, syncs state |
| tmux server | co-located with the daemon (`<CacheDir>/tmux.sock`) | one session per devcontainer; each pane runs `cld x exec … claude` |
| `cld x exec` | tmux pane (daemon side) | `docker exec`s into the target container and runs `claude` with a TTY |
| `claude` | inside each devcontainer | the actual Claude Code process, installed by the daemon at `/usr/local/bin/claude` |
| `cld x watch` / `cld x agent` / `cld x api` | inside each devcontainer | in-container helpers the daemon drives over `docker exec` (file watch, ssh-agent relay, **API relay**) |
| `cld it` / `cld up` / `cld ls` / `cld down` | wherever you invoke them | control-plane clients that talk to the daemon over `<CacheDir>/cld.sock` |

Key paths (`CacheDir` defaults to `$XDG_CACHE_HOME/cld`, i.e. `~/.cache/cld`):

- `<CacheDir>/cld.sock` — daemon control API (HTTP over a unix socket)
- `<CacheDir>/tmux.sock` — the dedicated tmux server
- `<CacheDir>/agent.sock`, `<CacheDir>/gitconfig` — host shares staged for relays

## Topology

The daemon is the only component with a docker client. It manages *sibling*
devcontainers; it never runs inside the container it is provisioning.

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

1. **resolve identity** — probe uid/gid/home/libc, compute the config dir
   (`~/.cld/claude`), workspace folder, and platform.
2. **install binaries** — copy `claude-<version>` and the `cld` binary into
   `/usr/local/bin` (atomic symlink swap; checksum-verified).
3. **prepare state** — restore the project backup, bootstrap credentials, install
   the host gitconfig, and seed onboarding/trust keys.
4. **create the session** — a host tmux session whose pane runs
   `cld x exec … -- claude` (see below).
5. **start watchers** — file-change sync, container watch, and the ssh-agent and
   **API** relays.

### How `claude` is launched

`ensure_session` builds the pane command and hands it to tmux. The remote argv
(`session_command`) resumes a prior conversation but **never lets a failed
resume kill the session**:

```
claude                                   # no prior transcript
sh -c 'claude --continue || exec claude' # prior transcript: resume, else fresh
```

This matters because `cld` cannot perfectly predict Claude Code's private
`projects/<encoded-cwd>` directory naming (it truncates and hashes long paths and
normalizes unicode). `internal/claude.EncodeProjectPath` mirrors that encoding
for backup/restore placement, but session *liveness* deliberately does not
depend on it: a resume Claude Code rejects degrades to a fresh session instead
of exiting with `no conversation found to continue`.

A pane's exit status is reported back to the daemon (`cld x exec` →
`POST /notify/exited?code=…`). A clean exit (0) becomes `session-ended`; a
non-zero exit becomes `failed` (kept visible, not silently reset), so crashes
are diagnosable instead of masquerading as a normal quit.

## Attaching (the existing paths)

`cld it <name>` asks the daemon where it lives (`GET /info`) and picks:

- **docker-exec attach** (`attach_via_exec`) — when the daemon runs in a
  container this host can see: `docker exec <daemon-ctr> tmux attach`. The host
  needs no tmux.
- **local attach** (`attach_local`) — when the daemon runs on this host:
  `tmux -S <tmux.sock> attach`.

Both need to reach the daemon's docker/tmux directly, which is fine from the
host but **not** from inside a managed devcontainer.

## State sync & relays (precedents)

- **Backups** (`internal/syncer`) — `projects/<enc>` transcripts and global
  credentials/settings are copied out on change and restored into fresh
  containers, rewriting the workspace path if it moved.
- **ssh-agent relay** (`internal/agentx`, `relay_agent`) — the daemon runs
  `cld x agent <sock>` in the container and **bridges that socket, over the
  `docker exec` stdio, to the host ssh-agent**. `agentx` is a *generic*
  multiplexing relay: a container-side unix socket ⇄ a duplex byte stream ⇄ a
  freshly-dialed socket on the daemon side. Only the daemon-side dial target is
  agent-specific.

That generic relay is exactly what the remote-control feature reuses.

---

## Remote control: in-container access (this change)

### Problem

The daemon's control API (`cld.sock`) and tmux server live on the daemon side
and are never mounted into a managed container. So `cld it`, `cld ls`, etc. run
*inside* a container have nothing to reach: `~/.cache/cld` is absent, and a
devcontainer normally has no route back to the daemon at all. The daemon only
ever reaches *into* containers over `docker exec`; nothing lets a container
reach back out.

Relaying the tmux socket instead does **not** work: tmux's client/server
protocol passes file descriptors over the socket (`SCM_RIGHTS`), which a byte
relay cannot carry. So the daemon must stream a pty, not proxy tmux.

### 1) Expose the control API inside the container

Reuse the ssh-agent mechanism, changing only the daemon-side dial target:

- **container side** — `cld x api <sock>` runs `agentx.ListenAndServe`, the same
  listener as `cld x agent`, on `~/.cache/cld/cld.sock` (the path an
  in-container `cld` dials for the daemon by default — so no configuration is
  needed there).
- **daemon side** — `relay_api` bridges each connection to a **per-container,
  self-scoped** control API served in-process (`scoped_api`), not to the full
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
`scoped_api(id)` with the owning container's id baked in — so requests can only
act on that container's own session (see security below). Now `cld ls`,
`cld it --new` (RecreateSession), `cld down` on itself, etc. work from inside
the container as plain HTTP calls over the relayed socket.

### 2) Attach over the control socket

Control is necessary but not sufficient: attaching needs a terminal. A new
endpoint streams a pty:

- **`GET /session/attach?name=&cols=&rows=&term=`** (`handle_attach`) — hijacks
  the connection, then runs `tmux attach-session` for `cld-<name>` **inside the
  daemon's own container** (`docker exec` provides the TTY and resize, via
  `termx.Stream`), piping it to the hijacked connection. Offered only when the
  daemon runs in a container (`Info.APIAttach = self_ctr != ""`).

Because the hijacked stream is carried by the same `agentx` relay, the client
needs neither docker nor tmux — just the one unix socket.

#### Wire protocol (after the HTTP `101` upgrade)

The single connection carries keystrokes *and* resizes, so the
client→daemon direction is framed; daemon→client is the raw pty.

```
client → daemon:
  'd' u32:len  <len bytes>     # stdin data
  'w' u16:cols u16:rows        # window resize
daemon → client:
  <raw pty bytes>              # terminal output
```

`read_attach_frames` decodes the framed side; resizes feed the `termx.Stream`
size channel (`ExecResize`); oversized/garbage frames are rejected. See
`internal/daemon/attach.go` and `internal/daemon/attach_test.go`.

### 3) `cld it` attach decision

```
FetchInfo():
  daemon in a container THIS host can see   → attach_via_exec  (docker exec tmux attach)   [host, unchanged]
  else, daemon offers APIAttach             → AttachSession    (stream over control socket) [in-container]
  else, daemon in an unreachable container  → attach_via_exec  (surfaces a clear error)
  else (daemon on this host)                → attach_local     (local tmux attach)
```

`daemon_container_reachable` distinguishes host from in-container by inspecting
the daemon's container id through the local docker client. Host behavior is
unchanged; the API path is taken only when the daemon's container isn't directly
reachable.

### Constraints & security

- **Self-scoped, no lateral movement.** The relayed API is `scoped_api(id)`, not
  the full control plane. `/items` returns only the caller's own container;
  `/session/attach`, `/session/new`, `/down`, and `/notify/exited` reject any
  request whose target is not the caller's own container. The identity is bound
  on the daemon side (the container id the relay serves), never supplied by the
  container, so untrusted code in one project cannot enumerate, attach to,
  recreate, or destroy another project's session. This matters because managed
  containers run agent-driven, potentially untrusted repository content.
- **Opt-out.** `auth.remote_control: false` disables the relay entirely
  (`RemoteControlEnabled`), symmetric to `forward_agent` for the ssh-agent
  relay. Default is on.
- **No new network surface.** The API is reachable in the container only through
  the daemon-initiated `docker exec` relay — only inside containers the daemon
  already provisions. There is no TCP listener and no token to leak.
- The relay socket path is resolved to match `os.UserCacheDir`
  (`$XDG_CACHE_HOME` or `$HOME/.cache`, probed per container in `resolve`), so an
  in-container `cld` finds it with no config. (An `XDG_CACHE_HOME` set only in an
  interactive shell rc — not the container environment — could still diverge.)
- API attach requires a **containerized daemon** (`self_ctr != ""`); a host-run
  daemon advertises `APIAttach = false`, so in-container attach falls through
  (host deployments attach from the host as before). Control commands
  (`ls`/`new`/`down`) work against either deployment.
- Same-arch only: the relays (like the watcher) run when the container arch
  matches the host, which is also when the in-container `cld` binary can run.
- The attach exec's lifetime is tied to the client connection (a per-attach
  context cancels it on disconnect), so dropped clients don't orphan tmux
  attaches on the daemon side.

### Tested

- `internal/daemon/attach_test.go` — attach frame codec (round-trip, oversized,
  bad type, non-blocking resize).
- `internal/daemon/integration_test.go` — “the daemon API is reachable from
  inside the container via the relay”: after provisioning, an in-container
  `cld ls` reaches the daemon through the relay and lists the container.
