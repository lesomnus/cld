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
- **The OAuth token is always user-default** — `Config.OAuthTokenStorePath()`
  sits next to `user-default/` under the same `DataDir`, set via `cld auth
  set-token`. It is never per-project and never in-container.
- **user-default never touches the host's real `~/.claude`.** cld does not
  read or write it; you populate `user-default/` yourself.
- **A change made inside a container only ever reaches that project's own
  per-project backup** — never another project's, and never user-default. So
  installing a skill inside one devcontainer cannot leak into a different
  project, and cannot become the new baseline everyone else gets.
- **`.credentials.json` is in-container only** (excluded from both
  user-default and per-project — see "Authentication" below), so one
  container's rotating OAuth session can never invalidate another's.

Below, `claude.Classify`'s bucket names (**Global**/**Project**/**Skip**) map
onto these tiers as: Global and Project both live under a project's
**per-project** dir (in different subdirectories); Skip means
**in-container**-only. "Global" is a legacy name — see the very next
paragraph for why it does *not* mean "shared across every devcontainer".

Every top-level entry in the config dir is classified by `claude.Classify`
into one of three buckets:

| Bucket | Backup location | Shared across devcontainers? |
|---|---|---|
| **Project** | `<project-backup>/projects/<enc>/`, `<project-backup>/file-history/` | only same-named containers |
| **Global** | `<project-backup>/settings/` | only same-named containers |
| **Skip** | not backed up | n/a |

`<project-backup>` is `internal/syncer.Layout.ProjectDir`, keyed by
devcontainer name (`Daemon.backup_key`) — the **same** directory for both
buckets. Despite the name, **Global is never a bucket shared across every
devcontainer**: it is a project-independent-*looking* category of state
(settings, not conversations) that still lands in that project's own isolated
backup, so a change made inside one project's container can only ever affect
that project's own future restores — never another project's. The classifier
is still an **allowlist** — only entries explicitly named in `global_entries`
go Global; anything else defaults to **Skip** — but that is to keep new Claude
Code directories from silently entering the backup at all the moment upstream
adds them, not to guard a shared bucket. The `✓ allow (global)` column below
marks exactly the entries on that allowlist.

## Entries produced by Claude Code

| Entry | Kind | Holds | Bucket | ✓ allow (global) |
|---|---|---|---|---|
| `.credentials.json` | file | claude.ai OAuth session (rotating refresh token) | Skip | |
| `.claude.json` | file | global config (`projects` map stripped on backup) | Global | ✓ |
| `settings.json` | file | user settings (model, permissions, hooks, …) | Global | ✓ |
| `settings.local.json` | file | machine-local settings overrides | Skip | |
| `CLAUDE.md` | file | global memory / instructions | Global | ✓ |
| `agents/` | dir | user-level subagent definitions | Global | ✓ |
| `commands/` | dir | user-level slash commands | Global | ✓ |
| `skills/` | dir | user-level skills | Global | ✓ |
| `output-styles/` | dir | user-level output styles | Global | ✓ |
| `plugins/` | dir | installed plugins / marketplaces | Global | ✓ |
| `projects/<enc>/` | dir | **conversation transcripts** (`*.jsonl`), keyed by workspace path | Project | |
| `file-history/` | dir | edited-file history / checkpoints | Project | |
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

`.credentials.json` is **not** shared. It holds a claude.ai OAuth session whose
refresh token rotates on every refresh, so one file shared across live containers
makes each container's refresh invalidate the others' — forcing repeated browser
logins. Instead, auth is injected per session as `CLAUDE_CODE_OAUTH_TOKEN` from a
long-lived token (`claude setup-token`), which no container refreshes, so there is
nothing to rotate or clobber. Set it with `cld auth set-token` (reads the token
from stdin; works from inside a devcontainer over the control-API relay) or via
`auth.oauth_token_file`; the daemon prefers the `set-token` value when present.

## Notes

- **Only the 8 checked entries are backed up at all** beyond transcripts/file
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
  to `global_entries`.
