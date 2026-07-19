// Package claude holds knowledge about Claude Code's on-disk state:
// the config dir layout, the project path encoding, and the state seeding
// that skips onboarding and trust prompts.
package claude

import (
	"bytes"
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

// cldBin is the absolute path of the in-container cld binary (install_dir/cld),
// used by the activity-reporting hooks. It is absolute so the hook works
// regardless of the session PATH, and kept STABLE across versions so the
// idempotent merge below recognizes an entry it wrote before.
const cldBin = "/usr/local/bin/cld"

// SeedSettings merges cld's own keys into an existing settings.json document
// (may be nil): the transcript retention floor, and hooks that report the live
// conversation activity to the daemon. cleanupPeriodDays must be large: cleanup
// is keyed on file mtime, which "docker cp" preserves, so restored transcripts
// would otherwise be pruned on the first start. The hooks let `cld ls` show
// working/waiting from an authoritative signal instead of scraping the pane.
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

	// A prompt submission means claude is about to generate (working); a Stop
	// means the turn finished (waiting); a Notification fires when claude pauses
	// for input — a permission prompt or 60s of idle — which is also a waiting
	// state and, crucially, corrects a turn that stopped without a Stop (e.g. a
	// permission prompt) so it doesn't stick at "working". A PostToolUse fires
	// after each tool result returns, which means claude is resuming work — this
	// is the ONLY signal that reports "working" after the user answers a mid-turn
	// prompt (a choice/AskUserQuestion or a permission approval): those arrive as
	// tool results, not a UserPromptSubmit, so without this the session sticks at
	// the "waiting" the Notification set until the next Stop. Its "working" always
	// precedes the turn-ending Stop, so a completed turn still settles on waiting.
	// The container's initial idle/waiting state is seeded by the daemon (see
	// ensure_), so no SessionStart hook is needed — one would wrongly report a
	// resume as idle.
	mergeCommandHook(doc, "UserPromptSubmit", activityHookCommand("working"))
	mergeCommandHook(doc, "PostToolUse", activityHookCommand("working"))
	mergeCommandHook(doc, "Stop", activityHookCommand("waiting"))
	mergeCommandHook(doc, "Notification", activityHookCommand("waiting"))

	return marshalIndent(doc)
}

// marshalIndent renders settings without Go's default HTML escaping, so the
// hook command's shell operators (&&, >) stay legible in settings.json instead
// of becoming && / >. The output is deterministic (json sorts map
// keys), which the idempotent seed relies on.
func marshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// activityHookCommand is the shell run by a lifecycle hook to report activity.
// It is guarded so a container without the cld binary (arch mismatch) or with
// the relay disabled (remote control off) fails silently rather than surfacing
// an error on every prompt and stop: the `[ -x ]` test skips a missing binary,
// output is discarded, and `|| true` forces a zero exit no matter what.
func activityHookCommand(state string) string {
	return "[ -x " + cldBin + " ] && " + cldBin + " x activity " + state + " >/dev/null 2>&1 || true"
}

// mergeCommandHook idempotently ensures settings.hooks[event] contains a
// matcher-less group whose command list holds {type:command, command}. It never
// touches, reorders, or removes the user's own hooks, and adding the same
// command twice is a no-op — so a re-provision produces byte-identical output
// and seed_file leaves the file alone. The doc is a json.Unmarshal result, so
// objects are map[string]any and arrays are []any; a wrong type assertion would
// silently drop user hooks, so every access is guarded.
func mergeCommandHook(doc map[string]any, event, command string) {
	// Only synthesize a hooks object when it is absent or null; a present value
	// of an unexpected type is the user's own data (however malformed) and must
	// not be clobbered, so bail rather than overwrite it.
	if raw, present := doc["hooks"]; !present || raw == nil {
		doc["hooks"] = map[string]any{}
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		return
	}

	// Same for the event's group list: an absent/null value is ours to create,
	// but a present non-array is the user's — leave it untouched.
	var groups []any
	if raw, present := hooks[event]; present && raw != nil {
		groups, ok = raw.([]any)
		if !ok {
			return
		}
	}

	// Already present under this event? Then it is a no-op.
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := gm["hooks"].([]any)
		for _, en := range entries {
			em, ok := en.(map[string]any)
			if !ok {
				continue
			}
			if em["type"] == "command" && em["command"] == command {
				return
			}
		}
	}

	// Append a NEW matcher-less group carrying only our command, leaving every
	// existing group untouched. UserPromptSubmit and Stop take no tool matcher,
	// so the "matcher" key is deliberately omitted.
	hooks[event] = append(groups, map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": command},
		},
	})
}
