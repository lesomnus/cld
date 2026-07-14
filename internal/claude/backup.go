package claude

import (
	"path"
	"strings"
)

// BackupClass tells where a file inside the config dir belongs in a backup.
type BackupClass int

const (
	// BackupSkip is regenerable or live-process state: it stays in-container
	// only and is never backed up.
	BackupSkip BackupClass = iota
	// BackupSettings is the project-independent-looking state: settings,
	// .claude.json, CLAUDE.md, agents, skills, and so on — see
	// settings_entries. It is backed up per project (see syncer.Layout),
	// never shared across containers, despite looking user-wide.
	BackupSettings
	// BackupTranscript is per-project conversation state: transcripts and
	// file history.
	BackupTranscript
)

// settings_entries are the config-dir entries that hold genuinely
// project-independent-looking state, distinguishing it from BackupTranscript
// (transcripts, file history). It is an allowlist on purpose: anything not
// named here defaults to BackupSkip rather than being backed up at all. A
// denylist would silently sweep every new Claude Code directory (jobs, tasks,
// backups, history.jsonl, caches, cld's own daemon files, …) into the backup
// the moment upstream adds it; an allowlist stays closed.
//
// The caller decides where a BackupSettings-classified file lands — cld's
// syncer package stores it under the same isolated per-project backup dir as
// BackupTranscript state, never a bucket shared across containers, so a
// change inside one project's container can never bleed into another's on
// restore.
//
// The head (first path segment) is matched, so both root files
// (".credentials.json") and whole trees ("agents/foo.md") are covered.
//   - .claude.json/settings.json: user-level config, so a fresh container
//     skips onboarding and keeps the user's model/permissions/hooks.
//   - CLAUDE.md and agents/commands/skills/output-styles/plugins: user-level
//     customizations meant to apply everywhere, not per project.
//   - .credentials.json: the claude.ai OAuth session from the per-container
//     login. Its refresh token rotates, so this must NOT be shared across live
//     containers — but a project backup is isolated (one live container per
//     project; see syncer.Layout), so persisting it here lets a recreated
//     container resume the same login instead of prompting a fresh one. Restore
//     only ever runs into a container with no state of its own, so a live
//     container's rotating token is never clobbered by an older backup.
var settings_entries = map[string]bool{
	".claude.json":      true,
	".credentials.json": true,
	"settings.json":     true,
	"CLAUDE.md":         true,
	"agents":            true,
	"commands":          true,
	"skills":            true,
	"output-styles":     true,
	"plugins":           true,
}

// Classify classifies a path relative to the config dir into transcript state
// (conversations, file history) vs. settings-looking state (see
// settings_entries); everything else — live-process state, caches,
// background-session records, cld's own runtime files — is skipped so it
// never reaches a backup.
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
		return BackupTranscript
	case settings_entries[head]:
		return BackupSettings
	default:
		return BackupSkip
	}
}
