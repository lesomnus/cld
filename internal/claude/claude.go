// Package claude holds knowledge about Claude Code's on-disk state:
// the config dir layout, the project path encoding, and the state seeding
// that skips onboarding and trust prompts.
package claude

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ConfigDirIn is where CLAUDE_CONFIG_DIR points inside a container: a
// cld-owned directory, deliberately NOT the default ~/.claude. Users commonly
// bind-mount a shared ~/.claude into every devcontainer; with all workspaces
// at the same in-container path (e.g. /workspace) that merges every project's
// conversations into one history. A dedicated directory keeps cld's per-project
// sync authoritative regardless of such mounts. It also puts .claude.json
// inside the backed-up directory instead of its default $HOME/.claude.json.
func ConfigDirIn(home string) string {
	return home + "/.cld/claude"
}

// LegacyConfigDirIn is claude's default config dir; used only to bootstrap
// credentials from a user's existing bind-mounted ~/.claude.
func LegacyConfigDirIn(home string) string {
	return home + "/.claude"
}

// EncodeProjectPath encodes a workspace path the way Claude Code names
// transcript directories under projects/: every non-alphanumeric CHARACTER
// becomes "-" (one dash per rune, so multibyte paths encode the same as
// Claude Code's own encoding). The encoding is lossy on purpose; it only
// needs to match.
func EncodeProjectPath(p string) string {
	var b strings.Builder
	b.Grow(len(p))
	for _, r := range p {
		is_alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if is_alnum {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// SeedState merges the onboarding and per-project trust keys into an existing
// .claude.json document (may be nil), so the first run skips the onboarding
// wizard and the workspace trust dialog. Existing keys are preserved.
// These keys are internal to Claude Code and may break across versions;
// if they do, the prompts simply show up again.
func SeedState(existing []byte, workspace string) ([]byte, error) {
	doc := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("parse existing state: %w", err)
		}
	}

	if _, ok := doc["hasCompletedOnboarding"]; !ok {
		doc["hasCompletedOnboarding"] = true
	}
	if _, ok := doc["theme"]; !ok {
		doc["theme"] = "dark"
	}

	projects, ok := doc["projects"].(map[string]any)
	if !ok {
		projects = map[string]any{}
		doc["projects"] = projects
	}
	project, ok := projects[workspace].(map[string]any)
	if !ok {
		project = map[string]any{}
		projects[workspace] = project
	}
	if _, ok := project["hasTrustDialogAccepted"]; !ok {
		project["hasTrustDialogAccepted"] = true
	}
	if _, ok := project["hasCompletedProjectOnboarding"]; !ok {
		project["hasCompletedProjectOnboarding"] = true
	}

	return json.MarshalIndent(doc, "", "  ")
}

// SeedSettings merges retention settings into an existing settings.json
// document (may be nil). cleanupPeriodDays must be large: cleanup is keyed
// on file mtime, which "docker cp" preserves, so restored transcripts would
// otherwise be pruned on the first start.
func SeedSettings(existing []byte) ([]byte, error) {
	doc := map[string]any{}
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, fmt.Errorf("parse existing settings: %w", err)
		}
	}

	if _, ok := doc["cleanupPeriodDays"]; !ok {
		doc["cleanupPeriodDays"] = 365
	}

	return json.MarshalIndent(doc, "", "  ")
}
