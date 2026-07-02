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

// skip_dirs are regenerable, live-process, or legacy state.
var skip_dirs = map[string]bool{
	"shell-snapshots": true,
	"sessions":        true,
	"session-env":     true,
	"debug":           true,
	"logs":            true,
	"todos":           true,
	"statsig":         true,
	"paste-cache":     true,
	"image-cache":     true,
}

// Classify classifies a path relative to the config dir.
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
	case skip_dirs[head]:
		return BackupSkip
	case head == "projects" || head == "file-history":
		return BackupProject
	default:
		return BackupGlobal
	}
}
