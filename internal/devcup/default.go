package devcup

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// defaultConfig is the minimal devcontainer.json used when a workspace has none.
// It is plain JSON (no comments) so it can be unmarshaled without the JSONC
// stripper, keeping this package free of a dependency on internal/devc.
//
//go:embed assets/devcontainer.json
var defaultConfig []byte

// WriteDefaultConfig renders the built-in minimal devcontainer.json with its
// "name" set to the workspace's directory basename, writes it to a fresh temp
// dir, and returns the file path plus a cleanup func. The workspace itself is
// left untouched; callers pass the path to `devcontainer up --override-config`.
func WriteDefaultConfig(workspace string) (path string, cleanup func(), err error) {
	var m map[string]any
	if err := json.Unmarshal(defaultConfig, &m); err != nil {
		return "", nil, fmt.Errorf("parse built-in default config: %w", err)
	}
	m["name"] = filepath.Base(workspace)

	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("render default config: %w", err)
	}

	dir, err := os.MkdirTemp("", "cld-devcontainer-*")
	if err != nil {
		return "", nil, err
	}
	p := filepath.Join(dir, "devcontainer.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return p, func() { os.RemoveAll(dir) }, nil
}
