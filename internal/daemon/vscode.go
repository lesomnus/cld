package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path"
	"reflect"

	"github.com/lesomnus/cld/internal/dockerx"
)

// vscodeProfileKey is the VS Code setting that lists custom terminal profiles.
const vscodeProfileKey = "terminal.integrated.profiles.linux"

// vscodeServerDirs are the editor server homes whose machine settings VS Code
// (and forks) read. cld seeds a "claude" terminal profile into each so opening
// a terminal from that profile drops straight into the session. Other editors
// (code-server, …) are covered by the documented devcontainer.json snippet.
var vscodeServerDirs = []string{".vscode-server", ".cursor-server"}

// merge_vscode_profile adds a "claude" terminal profile that runs `cld it` to a
// VS Code machine settings.json, preserving every other setting and profile.
// changed is false when the profile is already present and identical.
func merge_vscode_profile(existing []byte) (out []byte, changed bool, err error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(existing)) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, false, err
		}
	}

	want := map[string]any{"path": "cld", "args": []any{"it"}}
	profiles, _ := doc[vscodeProfileKey].(map[string]any)
	if profiles == nil {
		profiles = map[string]any{}
	} else if cur, ok := profiles["claude"]; ok && reflect.DeepEqual(cur, want) {
		return existing, false, nil
	}
	profiles["claude"] = want
	doc[vscodeProfileKey] = profiles

	out, err = json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// install_vscode_profile seeds the "claude" terminal profile into each editor's
// machine settings inside the container, proactively (creating the dir) so the
// profile is present when the editor connects. Best-effort: an editor that is
// never used just gets an unused settings file, and any failure is ignored.
// Only meaningful with the API relay (which `cld it` uses inside the container).
func (d *Daemon) install_vscode_profile(ctx context.Context, e *entry, id string) {
	if !d.cfg.Auth.RemoteControlEnabled() {
		return
	}
	for _, server := range vscodeServerDirs {
		machine := path.Join(e.home, server, "data", "Machine")
		settings := path.Join(machine, "settings.json")

		existing, _, err := dockerx.ReadFile(ctx, d.cli, id, settings)
		if err != nil {
			continue // transport error; try the next, don't fail provisioning
		}
		merged, changed, err := merge_vscode_profile(existing)
		if err != nil || !changed {
			continue
		}
		if _, code, err := dockerx.ExecOutput(ctx, d.cli, id, e.user, []string{"mkdir", "-p", machine}); err != nil || code != 0 {
			continue
		}
		if err := dockerx.WriteFile(ctx, d.cli, id, machine, "settings.json", 0o644, e.uid, e.gid, merged); err != nil {
			if ctx.Err() == nil {
				d.log.Warn("vscode profile: write failed",
					slog.String("name", e.item.Name), slog.String("editor", server))
			}
			continue
		}
		d.log.Info("vscode terminal profile installed",
			slog.String("name", e.item.Name), slog.String("editor", server))
	}
}
