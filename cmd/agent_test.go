package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

func TestStageClaudeConfig(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	claudeDir := filepath.Join(homeDir, ".claude")
	mkfile := func(rel, content string) {
		p := filepath.Join(claudeDir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	mkfile("settings.json", `{"model":"opus"}`)
	mkfile("CLAUDE.md", "be terse")
	mkfile("commands/hi.md", "hello")
	mkfile("commands/sub/deep.md", "deep")
	mkfile("agents/rev.md", "review")
	mkfile(".credentials.json", "secret") // excluded (never propagate credentials)
	mkfile("projects/-x/s.jsonl", "{}")    // excluded (per-project history)

	c := &config.Config{CacheDir: t.TempDir()}
	stageClaudeConfig(c)

	share := c.ClaudeShareDir()
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(share, rel))
		require.NoError(t, err)
		return string(b)
	}
	require.Equal(t, `{"model":"opus"}`, read("settings.json"))
	require.Equal(t, "be terse", read("CLAUDE.md"))
	require.Equal(t, "hello", read("commands/hi.md"))
	require.Equal(t, "deep", read("commands/sub/deep.md"))
	require.Equal(t, "review", read("agents/rev.md"))
	require.NoFileExists(t, filepath.Join(share, ".credentials.json"))
	require.NoDirExists(t, filepath.Join(share, "projects"))

	// A file removed on the host stops propagating (the stage is rebuilt).
	require.NoError(t, os.Remove(filepath.Join(claudeDir, "commands", "hi.md")))
	stageClaudeConfig(c)
	require.NoFileExists(t, filepath.Join(share, "commands", "hi.md"))
	require.FileExists(t, filepath.Join(share, "commands", "sub", "deep.md"))

	// Disabled: nothing is staged.
	c2 := &config.Config{CacheDir: t.TempDir()}
	no := false
	c2.Auth.ShareConfig = &no
	stageClaudeConfig(c2)
	require.NoDirExists(t, c2.ClaudeShareDir())
}

// TestStageClaudeConfigSkipsSymlinks ensures a symlink planted under a shared
// directory cannot pull an arbitrary host file into the stage.
func TestStageClaudeConfigSkipsSymlinks(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	// A secret outside ~/.claude, and a symlink to it under commands/.
	secret := filepath.Join(homeDir, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("TOP SECRET"), 0o600))
	commands := filepath.Join(homeDir, ".claude", "commands")
	require.NoError(t, os.MkdirAll(commands, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(commands, "ok.md"), []byte("fine"), 0o644))
	require.NoError(t, os.Symlink(secret, filepath.Join(commands, "leak.md")))

	c := &config.Config{CacheDir: t.TempDir()}
	stageClaudeConfig(c)

	share := c.ClaudeShareDir()
	require.FileExists(t, filepath.Join(share, "commands", "ok.md"))
	require.NoFileExists(t, filepath.Join(share, "commands", "leak.md"))
	// And the secret's content never landed in the stage under any name.
	got, _ := os.ReadFile(filepath.Join(share, "commands", "leak.md"))
	require.NotContains(t, string(got), "TOP SECRET")
}
