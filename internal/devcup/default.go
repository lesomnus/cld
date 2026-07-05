package devcup

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultConfig is the minimal devcontainer.json used when a workspace has none.
// It is plain JSON (no comments) so it can be unmarshaled without the JSONC
// stripper, keeping this package free of a dependency on internal/devc.
//
//go:embed assets/devcontainer.json
var defaultConfig []byte

// defaultConfigRel is where the built-in default is materialized inside a
// workspace: the standard location the devcontainer CLI, VS Code, and cld's
// daemon all read back from the devcontainer.config_file label.
const defaultConfigRel = ".devcontainer/devcontainer.json"

// WriteDefaultConfig renders the built-in minimal devcontainer.json with its
// "name" set to the workspace's directory basename and writes it into the
// workspace at .devcontainer/devcontainer.json, returning the written path.
//
// It writes into the workspace (rather than an ephemeral temp file passed via
// --override-config) so that the devcontainer.config_file label the CLI stamps
// on the container points at a real, host-readable file. VS Code re-reads that
// path on every open; a missing file makes the container list by name but
// refuse to open. Callers only invoke this for a workspace that HasConfig
// reports false, so it never clobbers a user's own config. The file is
// best-effort added to .git/info/exclude so it does not dirty git status.
func WriteDefaultConfig(workspace string) (string, error) {
	var m map[string]any
	if err := json.Unmarshal(defaultConfig, &m); err != nil {
		return "", fmt.Errorf("parse built-in default config: %w", err)
	}
	m["name"] = filepath.Base(workspace)

	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("render default config: %w", err)
	}

	dir := filepath.Join(workspace, ".devcontainer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "devcontainer.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return "", err
	}

	gitExclude(workspace, defaultConfigRel)
	return p, nil
}

// gitExclude appends rel to the workspace's .git/info/exclude so a
// cld-generated file does not show up as untracked. Best-effort: a no-op when
// the workspace is not a plain git repo (no .git directory) or rel is already
// excluded.
func gitExclude(workspace, rel string) {
	git := filepath.Join(workspace, ".git")
	if fi, err := os.Stat(git); err != nil || !fi.IsDir() {
		return // no .git dir (not a repo, or a worktree/submodule .git file)
	}
	info := filepath.Join(git, "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return
	}
	excl := filepath.Join(info, "exclude")

	existing, _ := os.ReadFile(excl)
	for line := range strings.SplitSeq(string(existing), "\n") {
		if strings.TrimSpace(line) == rel {
			return // already excluded
		}
	}

	f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	// Start the entry on its own line if the file does not end in a newline.
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		rel = "\n" + rel
	}
	fmt.Fprintln(f, rel)
}
