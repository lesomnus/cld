# Claude Code config-dir layout & backup classification

`cld` points `CLAUDE_CONFIG_DIR` at a dedicated directory inside each container
(`~/.cld/claude`, see `claude.ConfigDirIn`) instead of the default `~/.claude`.
Claude Code and `cld` both write state into that one directory. When `cld` backs
a container up, every top-level entry is classified by `claude.Classify` into
one of three buckets:

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
  history, and even those land in the same isolated per-project backup dir —
  never a bucket shared across every devcontainer. A container is only ever
  seeded from its *own* project's prior backup (same devcontainer name), never
  from another project's.
- cld's own **user-default** dir (`Config.UserDefaultDir`, under `DataDir` —
  see `docs/architecture.md`) is the actual source of truth for
  `settings.json`/`CLAUDE.md`/`agents/`/`commands/`/`output-styles/`, and it is
  **not** the host's `~/.claude` — cld never reads or writes that. The daemon
  mirrors user-default into every session on each provision
  (`install_claude_config`), sanitized and overwriting whatever a restore
  brought back. So editing files under user-default is the supported way to
  change those five for every project; a change made inside a container is
  backed up (for that project's own next restore) but does not become the new
  baseline for other projects.
- This list reflects what we currently know Claude Code writes. Since the
  classifier defaults unknown entries to **Skip**, a newly added Claude Code
  directory is safely excluded from the backup until it is deliberately added
  to `global_entries`.
