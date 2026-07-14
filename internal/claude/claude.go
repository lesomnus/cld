// Package claude holds knowledge about Claude Code's on-disk state:
// the config dir layout, the project path encoding, and the state seeding
// that skips onboarding and trust prompts.
package claude

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
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

// maxEncodedLen mirrors Claude Code's length threshold: an encoded path longer
// than this is truncated and disambiguated with a hash of the original path.
const maxEncodedLen = 200

// EncodeProjectPath encodes a workspace path the way Claude Code names its
// transcript directory under projects/, so cld reads and writes the very
// directory Claude Code does. It mirrors Claude Code's own function: replace
// every UTF-16 code unit that is not [A-Za-z0-9] with "-" (so an astral rune,
// two code units, becomes two dashes — matching JS String.replace), and, when
// the result exceeds maxEncodedLen, truncate it and append "-" plus a base-36
// hash of the original path.
//
// Matching Claude Code's private encoding is inherently brittle across its
// versions, so nothing that keeps a session alive may depend on this being
// exactly right: the launcher resumes with a fallback to a fresh session
// (see ensure_session). This only needs to be close enough that backups land
// where Claude Code looks for them.
func EncodeProjectPath(p string) string {
	units := utf16.Encode([]rune(p))
	var b strings.Builder
	b.Grow(len(units))
	for _, u := range units {
		if u >= 'a' && u <= 'z' || u >= 'A' && u <= 'Z' || u >= '0' && u <= '9' {
			b.WriteByte(byte(u))
		} else {
			b.WriteByte('-')
		}
	}
	enc := b.String()
	if len(enc) <= maxEncodedLen {
		return enc
	}
	return enc[:maxEncodedLen] + "-" + projectPathHash(units)
}

// projectPathHash mirrors Claude Code's disambiguating hash: a 32-bit rolling
// hash over the path's UTF-16 code units (t = t*31 + unit, wrapped to int32,
// i.e. JS `(t<<5)-t+charCodeAt|0`), rendered as base-36 of its absolute value.
func projectPathHash(units []uint16) string {
	var h int32
	for _, u := range units {
		h = h<<5 - h + int32(u)
	}
	v := int64(h)
	if v < 0 {
		v = -v
	}
	return strconv.FormatInt(v, 36)
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

// StripProjectState returns a .claude.json document reduced to its
// project-independent keys, dropping the per-project "projects" map. That map
// is keyed by the workspace path, which is the same in every cld devcontainer
// (e.g. /workspace), so keeping it in the shared global backup merges one
// project's prompt history and project-scoped settings into every other project
// on restore — the very cross-project bleed the dedicated config dir exists to
// prevent (see ConfigDirIn). Per-project transcripts live in the project
// backup, not here. A document that is not a JSON object is rejected (ok=false)
// so the caller can drop it rather than store an intact, leaky copy.
func StripProjectState(data []byte) ([]byte, bool) {
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil || doc == nil {
		return nil, false
	}
	delete(doc, "projects")
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, false
	}
	return out, true
}

// unsafeUserSettingsKeys are keys dropped from a host settings.json before it is
// installed into a container: they carry host secrets or run host-only binaries,
// or relax guardrails in a way that must not silently cross into a lower-trust
// sandbox.
//   - env holds arbitrary environment, a common home for API keys/tokens.
//   - apiKeyHelper/awsCredentialExport/awsAuthRefresh/otelHeadersHelper run host
//     binaries that do not exist in the container (like the gitconfig
//     credential.helper); auth comes instead from a per-container login (whose
//     credentials cld persists per project) or the opt-in broker proxy.
//   - enableAllProjectMcpServers/enabledMcpjsonServers auto-trust a repo's own
//     MCP servers; that trust should be re-decided in the sandbox, not inherited.
var unsafeUserSettingsKeys = []string{
	"env",
	"apiKeyHelper", "awsCredentialExport", "awsAuthRefresh", "otelHeadersHelper",
	"enableAllProjectMcpServers", "enabledMcpjsonServers",
}

// SanitizeUserSettings prepares a host settings.json for installation into a
// container. ok is false when the content is not a JSON object — the caller then
// skips it rather than letting an unparseable file reach (and fail) the settings
// seed, which would block every session. Otherwise the unsafe keys above are
// dropped and the remaining presentation/workflow keys (model, permissions,
// hooks, statusLine, output style, …) pass through.
func SanitizeUserSettings(data []byte) ([]byte, bool) {
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil || doc == nil {
		return nil, false
	}
	for _, k := range unsafeUserSettingsKeys {
		delete(doc, k)
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, false
	}
	return out, true
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
