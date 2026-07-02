// Package devc holds the devcontainer domain knowledge:
// which labels identify a devcontainer, how to derive its display name,
// its workspace path inside the container, and its effective user.
package devc

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

const (
	// LabelLocalFolder is set by the devcontainer CLI to the host-side
	// workspace path. Its presence is what identifies a devcontainer;
	// in compose projects only the primary service carries it.
	LabelLocalFolder = "devcontainer.local_folder"
	// LabelConfigFile is the host-side path of the devcontainer.json.
	LabelConfigFile = "devcontainer.config_file"
	// LabelMetadata is inherited from the image; a JSON array of merged
	// config/feature snippets. It does NOT contain workspaceFolder.
	LabelMetadata = "devcontainer.metadata"
	// LabelIgnore marks a container to be left alone by cld.
	LabelIgnore = "cld.ignore"
)

// DisplayName derives the user-facing name from the host-side workspace path.
// Container names are not used: non-compose devcontainers get random names
// and compose project names vary with COMPOSE_PROJECT_NAME.
func DisplayName(local_folder string) string {
	return filepath.Base(filepath.Clean(local_folder))
}

// SessionName is the tmux session name for a display name.
// "." and ":" collide with tmux target syntax so they are replaced.
func SessionName(name string) string {
	r := strings.NewReplacer(".", "_", ":", "_")
	return "cld-" + r.Replace(name)
}

// RemoteUser extracts the effective user from the devcontainer.metadata label.
// The label is a JSON array of config snippets; the last remoteUser wins.
// Returns "" if the label is absent or holds no remoteUser.
func RemoteUser(metadata string) string {
	if metadata == "" {
		return ""
	}

	var entries []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(metadata), &entries); err != nil {
		return ""
	}

	u := ""
	for _, e := range entries {
		raw, ok := e["remoteUser"]
		if !ok {
			continue
		}
		var v string
		if err := json.Unmarshal(raw, &v); err == nil && v != "" {
			u = v
		}
	}
	return u
}

// Mount is the subset of a container mount point needed to resolve the workspace.
type Mount struct {
	Source      string
	Destination string
}

// WorkspaceFolder resolves the workspace path inside the container.
// A workspaceFolder explicitly set in devcontainer.json wins; otherwise the
// mount whose source is the local folder tells where it landed. A
// workspaceFolder that still contains unresolvable ${...} variables after
// expansion is discarded in favor of the mount, since it is not a real path.
func WorkspaceFolder(config_file []byte, local_folder string, mounts []Mount) string {
	if len(config_file) > 0 {
		var c struct {
			WorkspaceFolder string `json:"workspaceFolder"`
		}
		if err := json.Unmarshal(StripJSONC(config_file), &c); err == nil && c.WorkspaceFolder != "" {
			wf := expandDevcontainerVars(c.WorkspaceFolder, local_folder)
			if !strings.Contains(wf, "${") {
				return wf
			}
		}
	}

	want := filepath.Clean(local_folder)
	for _, m := range mounts {
		if filepath.Clean(m.Source) == want {
			return m.Destination
		}
	}
	return ""
}

// expandDevcontainerVars expands the devcontainer.json variables that resolve
// to a path from host-side information alone. Container- or env-dependent
// variables are left untouched (and cause the value to be rejected above).
func expandDevcontainerVars(s string, local_folder string) string {
	base := filepath.Base(filepath.Clean(local_folder))
	r := strings.NewReplacer(
		"${localWorkspaceFolder}", local_folder,
		"${localWorkspaceFolderBasename}", base,
		"${containerWorkspaceFolderBasename}", base,
	)
	return r.Replace(s)
}
