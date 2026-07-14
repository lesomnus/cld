# Claude Code config-dir layout & backup classification

`cld` points `CLAUDE_CONFIG_DIR` at a dedicated directory inside each container
(`~/.cld/claude`, see `claude.ConfigDirIn`) instead of the default `~/.claude`.
Claude Code and `cld` both write state into that one directory.

## The three tiers

Claude Code state lives in exactly three places, each with different
ownership and a one-way flow:

| Tier | Owner | Location | Flows in from | Flows out to |
|---|---|---|---|---|
| **user-default** | you, directly | `Config.UserDefaultDir()` (`<DataDir>/user-default/`) | nothing — you edit it yourself | mirrored into **every** container on each provision (`install_claude_config`), overwriting whatever a restore brought back |
| **per-project** | cld, one dir per devcontainer name | `Config.ProjectBackupDir(key)` (`<DataDir>/projects/<key>/`) | that *same* project's own container, live (`internal/daemon/sync.go` watcher/poll → `copy_out`) | restored into a fresh container of the *same* project only (`syncer.CopyIn`); survives `cld down`, deleted only by `cld purge` |
| **in-container** | the container | `~/.cld/claude/...` inside the container (entries `claude.Classify` marks `BackupSkip`) | nothing outside ever writes it | nowhere — never leaves the container, never restored |

Invariants that follow from this:
- **The default login is per-project, not global.** A container logs in for
  itself and its `.credentials.json` is persisted to that project's own
  per-project backup and restored on recreate. Because the backup is isolated
  (one live container per project), one container's rotating OAuth session can
  never invalidate another's — the very clash a globally-shared file would cause.
- **The broker refresh token is host-side only** — the opt-in broker login
  (`cld auth login`, `Config.BrokerCredentialsPath()`) sits next to
  `user-default/` under `DataDir`. A broker session receives only a short-lived
  access token through the proxy; the rotating refresh token never leaves the
  daemon host, and never enters a container.
- **user-default never touches the host's real `~/.claude`.** cld does not
  read or write it; you populate `user-default/` yourself.
- **A change made inside a container only ever reaches that project's own
  per-project backup** — never another project's, and never user-default. So
  installing a skill inside one devcontainer cannot leak into a different
  project, and cannot become the new baseline everyone else gets.

Below, `claude.Classify`'s bucket names (**Settings**/**Transcript**/**Skip**)
map onto these tiers as: Settings and Transcript both live under a project's
**per-project** dir, in different subdirectories — they're split only because
they need different fetch/restore handling and independent dirty-tracking
(see "Why two buckets for one tier?" below), not because they go to different
places. Skip means **in-container**-only.

Every top-level entry in the config dir is classified by `claude.Classify`
into one of three buckets:

| Bucket | Backup location | Shared across devcontainers? |
|---|---|---|
| **Transcript** | `<project-backup>/projects/<enc>/`, `<project-backup>/file-history/` | only same-named containers |
| **Settings** | `<project-backup>/settings/` | only same-named containers |
| **Skip** | not backed up | n/a |

`<project-backup>` is `internal/syncer.Layout.ProjectDir`, keyed by
devcontainer name (`Daemon.backup_key`) — the **same** directory for both
buckets. Settings is a project-independent-*looking* category of state
(settings, not conversations), but it still lands in that project's own
isolated backup, so a change made inside one project's container can only
ever affect that project's own future restores — never another project's.
The classifier is an **allowlist** — only entries explicitly named in
`settings_entries` go Settings; anything else defaults to **Skip** — so a new
Claude Code directory never silently enters the backup the moment upstream
adds it. The `✓ allow (settings)` column below marks exactly the entries on
that allowlist.

### Why two buckets for one tier?

Settings and Transcript both end up under the same per-project dir, so why
not one bucket? Because despite the shared destination, they need genuinely
different handling:
- **Fetch**: Transcript is two fixed, known paths (`projects/<enc>`,
  `file-history`), copied out directly. Settings is a variable set of
  top-level names, so it's `ls`'d and filtered against the allowlist first.
- **Dirty-tracking**: the daemon tracks them with separate `dirty.settings`
  / `dirty.transcript` flags so a `settings.json` edit doesn't trigger
  re-fetching a (potentially huge) transcript tree, and vice versa.
- **Restore**: Settings is merged in wholesale. Transcript needs its encoded
  directory renamed and the `cwd` strings inside `.jsonl` files rewritten
  when the workspace path has moved (see `write_backup`'s `rewrite` param).
- **Sanitization**: only Settings' `.claude.json` gets its `projects` map
  stripped (`sanitize_settings_state`) before being stored.

## Entries produced by Claude Code

| Entry | Kind | Holds | Bucket | ✓ allow (settings) |
|---|---|---|---|---|
| `.credentials.json` | file | claude.ai OAuth session (rotating refresh token) | Settings | ✓ |
| `.claude.json` | file | user-level config (`projects` map stripped on backup) | Settings | ✓ |
| `settings.json` | file | user settings (model, permissions, hooks, …) | Settings | ✓ |
| `settings.local.json` | file | machine-local settings overrides | Skip | |
| `CLAUDE.md` | file | user-level memory / instructions | Settings | ✓ |
| `agents/` | dir | user-level subagent definitions | Settings | ✓ |
| `commands/` | dir | user-level slash commands | Settings | ✓ |
| `skills/` | dir | user-level skills | Settings | ✓ |
| `output-styles/` | dir | user-level output styles | Settings | ✓ |
| `plugins/` | dir | installed plugins / marketplaces | Settings | ✓ |
| `projects/<enc>/` | dir | **conversation transcripts** (`*.jsonl`), keyed by workspace path | Transcript | |
| `file-history/` | dir | edited-file history / checkpoints | Transcript | |
| `jobs/` | dir | **FleetView background-session records** (`state.json`, `timeline.jsonl`) | Skip | |
| `tasks/` | dir | subagent task transcripts for background jobs | Skip | |
| `backups/` | dir | Claude Code file-edit backups | Skip | |
| `history.jsonl` | file | REPL input history (typed prompts) | Skip | |
| `plans/` | dir | saved plan-mode plans | Skip | |
| `sessions/` | dir | live/resumable session state (daemon-backed) | Skip | |
| `session-env/` | dir | per-session environment | Skip | |
| `shell-snapshots/` | dir | captured shell state | Skip | |
| `todos/` | dir | per-session todo lists | Skip | |
| `telemetry/` | dir | telemetry spool | Skip | |
| `statsig/` | dir | feature-flag cache | Skip | |
| `cache/` | dir | misc cache | Skip | |
| `paste-cache/` | dir | pasted-content cache | Skip | |
| `image-cache/` | dir | image cache | Skip | |
| `debug/` | dir | debug logs | Skip | |
| `logs/` | dir | logs | Skip | |
| `mcp-needs-auth-cache.json` | file | MCP auth-needed cache | Skip | |
| `.last-cleanup` | file | cleanup timestamp | Skip | |

## Entries produced by cld (not Claude Code)

These live in the same directory but are `cld`'s own runtime state. None are on
the allowlist, so all are Skip.

| Entry | Holds | Bucket |
|---|---|---|
| `daemon/` | cld daemon state | Skip |
| `daemon.lock` | daemon lock (also matched by the `.lock` skip rule) | Skip |
| `daemon.log` | daemon log | Skip |
| `daemon.status.json` | daemon status | Skip |
| `daemon-auth-cooldown` | auth backoff state | Skip |
| `daemon-auth-status.json` | auth status | Skip |
| `agent.sock` | daemon control socket | Skip |
| `gitconfig` | seeded git config | Skip |

## Authentication

`.credentials.json` holds a claude.ai OAuth session whose refresh token rotates
on every refresh, so *sharing one file across live containers* makes each
container's refresh invalidate the others' — forcing repeated logins. cld avoids
that clash without giving up persistence, via two paths:

1. **Per-container login (default).** A session starts unauthenticated and
   Claude Code prompts a login inside the container. That `.credentials.json` is
   classified **Settings**, so it is backed up to *this project's own* isolated
   per-project dir and restored into a recreated container of the same project —
   you log in once per project, not once per container lifetime. This is safe
   precisely because the backup is per-project and there is normally one live
   container per project: no two live containers share the file, so no rotation
   clash. Restore only ever runs into a container with no state of its own, so an
   older backup never clobbers a live container's freshly-rotated token.
2. **Broker — `cld auth login` + `--proxy` (opt-in).** To share ONE
   Claude-subscription login across sessions, the daemon owns a single login
   (`Config.BrokerCredentialsPath()`, host-side, mode 0600), refreshes it
   centrally at the subscription token endpoint, and an opted-in session reaches
   Anthropic through a per-container reverse proxy (`internal/broker`, injected as
   `ANTHROPIC_BASE_URL`) that rewrites the `Authorization` header with the current
   short-lived access token. No container ever holds the refresh token, and
   because the proxy swaps in a fresh token per request, a long-running session
   never restarts when the token rotates. `cld auth login` runs `claude auth
   login` against a throwaway config dir, so the login it mints is a *separate*
   lineage the daemon owns from birth — the host's own `~/.claude` is never
   touched. (`cld auth login --from <file>` instead imports an existing
   credentials file, *moving* it — the source is deleted so the host and daemon
   can't share one refresh token.) The proxy is **opt-in per project**: a session
   uses it only when the project enabled it with `cld up --proxy` / `cld it
   --proxy` (remembered under `Config.ProxyStateDir()`), a broker login exists,
   and the in-container relay can run (`auth.remote_control`, arch match). It is
   opt-in because pointing `ANTHROPIC_BASE_URL` at a non-first-party endpoint
   makes Claude Code degrade its UI and disable some features; `--no-proxy`
   switches a project back to (1). While a project is in proxy mode the container
   never logs in, so it writes no `.credentials.json` to persist.

## Notes

- **Only the 9 checked entries are backed up at all** beyond transcripts/file
  history — see "The three tiers" above for where each tier lives and how
  state flows between them.
- `settings.json`/`CLAUDE.md`/`agents/`/`commands/`/`output-styles/` are also
  installed from user-default on every provision (`install_claude_config`),
  overwriting whatever a per-project restore brought back for those five —
  so editing user-default is the supported way to change them for every
  project at once.
- This list reflects what we currently know Claude Code writes. Since the
  classifier defaults unknown entries to **Skip**, a newly added Claude Code
  directory is safely excluded from the backup until it is deliberately added
  to `settings_entries`.
