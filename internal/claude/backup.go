package claude

import (
	"path"
	"strings"
)

// BackupClass tells where a file inside the config dir belongs in a backup.
type BackupClass int

const (
	// BackupSkip is regenerable or live-process state.
	BackupSkip BackupClass = iota
	// BackupGlobal is project-independent state: credentials, settings,
	// .claude.json, CLAUDE.md, agents, skills, and so on.
	BackupGlobal
	// BackupProject is per-project state: transcripts and file history.
	BackupProject
)

// global_entries are the config-dir entries that hold genuinely
// project-independent state and so belong in the shared global backup that
// every devcontainer restores from. It is an allowlist on purpose: the global
// backup is one directory shared by all containers (see Layout.GlobalDir), so
// anything not named here defaults to BackupSkip rather than bleeding across
// containers. A denylist would silently sweep every new Claude Code directory
// (jobs, tasks, backups, history.jsonl, caches, cld's own daemon files, …) into
// that shared bucket the moment upstream adds it; an allowlist stays closed.
//
// The head (first path segment) is matched, so both root files
// (".credentials.json") and whole trees ("agents/foo.md") are covered.
//   - .claude.json/settings.json: global config, so a fresh container skips
//     onboarding and keeps the user's model/permissions/hooks.
//   - CLAUDE.md and agents/commands/skills/output-styles/plugins: user-level
//     customizations meant to apply everywhere, not per project.
//
// Note .credentials.json is deliberately NOT here. It holds a claude.ai OAuth
// session whose refresh token rotates: sharing one file across live containers
// makes each container's refresh invalidate the others', forcing repeated
// browser logins. Auth is instead injected per session as CLAUDE_CODE_OAUTH_TOKEN
// from a long-lived token (see `cld auth set-token` / auth.oauth_token_file),
// which no container refreshes, so there is nothing to rotate or clobber.
var global_entries = map[string]bool{
	".claude.json":  true,
	"settings.json": true,
	"CLAUDE.md":     true,
	"agents":        true,
	"commands":      true,
	"skills":        true,
	"output-styles": true,
	"plugins":       true,
}

// Classify classifies a path relative to the config dir. project state
// (transcripts, file history) is keyed per project; the global allowlist above
// is shared across containers; everything else — live-process state, caches,
// background-session records, cld's own runtime files — is skipped so it never
// reaches a backup and never bleeds between devcontainers.
func Classify(rel string) BackupClass {
	rel = path.Clean(strings.TrimPrefix(rel, "./"))
	if rel == "." || rel == "" {
		return BackupSkip
	}

	if strings.HasSuffix(rel, ".lock") || strings.HasSuffix(rel, ".tmp") {
		return BackupSkip
	}

	head := rel
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		head = rel[:i]
	}

	switch {
	case head == "projects" || head == "file-history":
		return BackupProject
	case global_entries[head]:
		return BackupGlobal
	default:
		return BackupSkip
	}
}
