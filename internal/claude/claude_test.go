package claude_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lesomnus/cld/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestEncodeProjectPath(t *testing.T) {
	t.Run("slashes", func(t *testing.T) {
		require.Equal(t, "-workspace", claude.EncodeProjectPath("/workspace"))
	})
	t.Run("every non-alphanumeric character", func(t *testing.T) {
		require.Equal(t, "-home-a-b-c-1", claude.EncodeProjectPath("/home/a_b.c 1"))
	})
	t.Run("one dash per UTF-16 code unit, matching Claude Code", func(t *testing.T) {
		// A BMP Hangul syllable is one UTF-16 unit -> one dash.
		require.Equal(t, "-workspaces--", claude.EncodeProjectPath("/workspaces/가"))
		// An astral rune (emoji) is a surrogate pair, two UTF-16 units, so
		// Claude Code's JS String.replace emits two dashes; cld must match.
		require.Equal(t, "-workspaces---", claude.EncodeProjectPath("/workspaces/😀"))
	})
	t.Run("long paths are truncated and hashed like Claude Code", func(t *testing.T) {
		// Ground truth from Claude Code's own WS()/hash on this input.
		in := "/" + strings.Repeat("a", 250)
		want := "-" + strings.Repeat("a", 199) + "-feo44x"
		require.Equal(t, want, claude.EncodeProjectPath(in))
	})
}

func TestSeedState(t *testing.T) {
	parse := func(t *testing.T, data []byte) map[string]any {
		var v map[string]any
		require.NoError(t, json.Unmarshal(data, &v))
		return v
	}

	t.Run("from scratch", func(t *testing.T) {
		out, err := claude.SeedState(nil, "/workspace")
		require.NoError(t, err)

		v := parse(t, out)
		require.Equal(t, true, v["hasCompletedOnboarding"])
		p := v["projects"].(map[string]any)["/workspace"].(map[string]any)
		require.Equal(t, true, p["hasTrustDialogAccepted"])
		require.Equal(t, true, p["hasCompletedProjectOnboarding"])
	})
	t.Run("existing keys are preserved", func(t *testing.T) {
		in := []byte(`{"theme":"light","projects":{"/workspace":{"hasTrustDialogAccepted":false},"/other":{"x":1}}}`)
		out, err := claude.SeedState(in, "/workspace")
		require.NoError(t, err)

		v := parse(t, out)
		require.Equal(t, "light", v["theme"])
		require.Equal(t, true, v["hasCompletedOnboarding"])
		projects := v["projects"].(map[string]any)
		require.Equal(t, false, projects["/workspace"].(map[string]any)["hasTrustDialogAccepted"])
		require.Contains(t, projects, "/other")
	})
	t.Run("invalid existing document", func(t *testing.T) {
		_, err := claude.SeedState([]byte("nope"), "/workspace")
		require.Error(t, err)
	})
}

func TestSeedSettings(t *testing.T) {
	t.Run("sets retention", func(t *testing.T) {
		out, err := claude.SeedSettings(nil)
		require.NoError(t, err)

		var v map[string]any
		require.NoError(t, json.Unmarshal(out, &v))
		require.Equal(t, float64(365), v["cleanupPeriodDays"])
	})
	t.Run("existing retention is preserved", func(t *testing.T) {
		out, err := claude.SeedSettings([]byte(`{"cleanupPeriodDays":7,"model":"opus"}`))
		require.NoError(t, err)

		var v map[string]any
		require.NoError(t, json.Unmarshal(out, &v))
		require.Equal(t, float64(7), v["cleanupPeriodDays"])
		require.Equal(t, "opus", v["model"])
	})
}

func TestClassify(t *testing.T) {
	t.Run("transcript state", func(t *testing.T) {
		require.Equal(t, claude.BackupTranscript, claude.Classify("projects/-workspace/abc.jsonl"))
		require.Equal(t, claude.BackupTranscript, claude.Classify("file-history/xyz/1"))
	})
	t.Run("settings state", func(t *testing.T) {
		require.Equal(t, claude.BackupSettings, claude.Classify(".claude.json"))
		require.Equal(t, claude.BackupSettings, claude.Classify("settings.json"))
		// Credentials are persisted per project (the backup is isolated, one live
		// container per project), so a recreated container resumes the login.
		require.Equal(t, claude.BackupSettings, claude.Classify(".credentials.json"))
		require.Equal(t, claude.BackupSettings, claude.Classify("agents/foo.md"))
		require.Equal(t, claude.BackupSettings, claude.Classify("CLAUDE.md"))
		require.Equal(t, claude.BackupSettings, claude.Classify("skills/x/SKILL.md"))
		require.Equal(t, claude.BackupSettings, claude.Classify("plugins/p/manifest.json"))
	})
	t.Run("skipped state", func(t *testing.T) {
		require.Equal(t, claude.BackupSkip, claude.Classify("shell-snapshots/x"))
		require.Equal(t, claude.BackupSkip, claude.Classify("sessions/1.json"))
		require.Equal(t, claude.BackupSkip, claude.Classify("statsig/cache"))
		require.Equal(t, claude.BackupSkip, claude.Classify("todos/old.json"))
		require.Equal(t, claude.BackupSkip, claude.Classify("foo.lock"))
		require.Equal(t, claude.BackupSkip, claude.Classify("."))
	})
	t.Run("unknown entries are skipped, not shared globally", func(t *testing.T) {
		// Background-session and other per-container state must never reach the
		// shared global backup, else completed sessions from one devcontainer
		// surface in another's FleetView.
		require.Equal(t, claude.BackupSkip, claude.Classify("jobs/85f00019/state.json"))
		require.Equal(t, claude.BackupSkip, claude.Classify("tasks/abc/1.json"))
		require.Equal(t, claude.BackupSkip, claude.Classify("backups/x"))
		require.Equal(t, claude.BackupSkip, claude.Classify("history.jsonl"))
		require.Equal(t, claude.BackupSkip, claude.Classify("plans/p.md"))
		require.Equal(t, claude.BackupSkip, claude.Classify("telemetry/t"))
		require.Equal(t, claude.BackupSkip, claude.Classify("daemon/x"))
		require.Equal(t, claude.BackupSkip, claude.Classify("agent.sock"))
	})
	t.Run("temp files are skipped even under projects", func(t *testing.T) {
		require.Equal(t, claude.BackupSkip, claude.Classify("projects/-workspace/s1.jsonl.tmp"))
		require.Equal(t, claude.BackupSkip, claude.Classify("projects/-workspace/x.lock"))
	})
}

func TestStripProjectState(t *testing.T) {
	t.Run("drops the per-project projects map, keeps global keys", func(t *testing.T) {
		in := []byte(`{
			"oauthAccount":{"emailAddress":"a@b.c"},
			"userID":"u1",
			"mcpServers":{"user-scoped":{}},
			"projects":{"/workspace":{"history":[{"display":"secret prompt"}]}}
		}`)
		out, ok := claude.StripProjectState(in)
		require.True(t, ok)

		var doc map[string]any
		require.NoError(t, json.Unmarshal(out, &doc))
		require.NotContains(t, doc, "projects", "per-project state must be stripped")
		require.NotContains(t, string(out), "secret prompt", "no project's history may survive")
		require.Contains(t, doc, "oauthAccount")
		require.Equal(t, "u1", doc["userID"])
		require.Contains(t, doc, "mcpServers")
	})
	t.Run("a document without projects round-trips ok", func(t *testing.T) {
		out, ok := claude.StripProjectState([]byte(`{"userID":"u1"}`))
		require.True(t, ok)
		require.JSONEq(t, `{"userID":"u1"}`, string(out))
	})
	t.Run("non-object content is rejected so it is dropped, not stored intact", func(t *testing.T) {
		for _, bad := range []string{`not json`, `[1,2]`, `null`, `5`, ``} {
			_, ok := claude.StripProjectState([]byte(bad))
			require.Falsef(t, ok, "input %q should be rejected", bad)
		}
	})
}

func TestSanitizeUserSettings(t *testing.T) {
	t.Run("drops secret/host-only/guardrail keys, keeps workflow keys", func(t *testing.T) {
		in := []byte(`{
			"apiKeyHelper":"/host/bin/key",
			"awsAuthRefresh":"x", "otelHeadersHelper":"y",
			"env":{"ANTHROPIC_API_KEY":"sk-secret","FOO":"bar"},
			"enableAllProjectMcpServers":true, "enabledMcpjsonServers":["evil"],
			"model":"opus", "permissions":{"allow":["Bash"]}, "outputStyle":"terse"
		}`)
		out, ok := claude.SanitizeUserSettings(in)
		require.True(t, ok)
		var doc map[string]any
		require.NoError(t, json.Unmarshal(out, &doc))
		for _, k := range []string{"apiKeyHelper", "awsAuthRefresh", "otelHeadersHelper", "env", "enableAllProjectMcpServers", "enabledMcpjsonServers"} {
			require.NotContainsf(t, doc, k, "%s must be stripped", k)
		}
		require.NotContains(t, string(out), "sk-secret", "no secret from env may survive")
		require.Equal(t, "opus", doc["model"])
		require.Contains(t, doc, "permissions")
		require.Equal(t, "terse", doc["outputStyle"])
	})
	t.Run("a clean object round-trips ok", func(t *testing.T) {
		out, ok := claude.SanitizeUserSettings([]byte(`{"model":"opus"}`))
		require.True(t, ok)
		require.JSONEq(t, `{"model":"opus"}`, string(out))
	})
	t.Run("non-object content is rejected (ok=false) so it is skipped", func(t *testing.T) {
		for _, bad := range []string{`not json`, `{"a":1,}`, `[1,2]`, `null`, `5`} {
			_, ok := claude.SanitizeUserSettings([]byte(bad))
			require.Falsef(t, ok, "input %q should be rejected", bad)
		}
	})
}
