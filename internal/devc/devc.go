// Package devc holds the devcontainer domain knowledge:
// which labels identify a devcontainer, how to derive its display name,
// its workspace path inside the container, and its effective user.
package devc

import (
	"encoding/json"
	"hash/fnv"
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

// ProjectName returns the "name" field from a devcontainer.json, or "" if it
// is absent or the document cannot be parsed.
func ProjectName(config_file []byte) string {
	if len(config_file) == 0 {
		return ""
	}
	var c struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(StripJSONC(config_file), &c); err != nil {
		return ""
	}
	return strings.TrimSpace(c.Name)
}

// Slug turns an arbitrary name into a single, filesystem- and tmux-safe path
// component: any run of characters outside [A-Za-z0-9._-] becomes a single
// "-", and leading/trailing "-" and "." are trimmed. Returns "" for a name
// that slugs to nothing.
func Slug(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	dash := false
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-.")
}

// SessionName is the tmux session name for a display name.
// "." and ":" collide with tmux target syntax so they are replaced.
func SessionName(name string) string {
	r := strings.NewReplacer(".", "_", ":", "_")
	return "cld-" + r.Replace(name)
}

// aliasShort is the length at or below which a name is already terse enough to
// serve as its own alias, and the length a longer single word is truncated to.
const aliasShort = 6

// Alias derives a short, readable handle from a display name: the name itself
// when it is already short, the initials of its segments when it is a
// multi-word name (split on "-", "_", "."), or a truncation of a long single
// word otherwise. The result is lowercased and slug-safe. It is deterministic
// — the same name always yields the same alias — and is NOT unique on its own;
// callers disambiguate collisions with Fingerprint. Returns "" for a name that
// slugs to nothing.
func Alias(name string) string {
	s := strings.ToLower(Slug(name))
	if s == "" {
		return ""
	}
	if len(s) <= aliasShort {
		return s
	}

	segs := strings.FieldsFunc(s, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	if len(segs) >= 2 {
		var b strings.Builder
		for _, seg := range segs {
			b.WriteByte(seg[0]) // FieldsFunc never yields empty fields
		}
		if init := b.String(); len(init) >= 2 {
			return init
		}
	}
	return s[:aliasShort]
}

// crockfordLower is Crockford's base32 alphabet in lowercase: the digits and
// letters minus i, l, o, u, so a fingerprint never reads as an ambiguous or
// unintended word.
const crockfordLower = "0123456789abcdefghjkmnpqrstvwxyz"

// Fingerprint returns a short, stable, lowercase base32 (Crockford) digest of
// seed. It is deterministic — the same seed always yields the same digits, with
// no randomness — so it disambiguates colliding aliases while staying derived
// from the container's identity. High-order digits come first, so a prefix of
// the result still spreads well. Returns "0" for an empty seed.
func Fingerprint(seed string) string {
	h := fnv.New64a()
	h.Write([]byte(seed))
	v := h.Sum64()

	var b [13]byte // ceil(64 / 5) = 13 base32 digits
	n := len(b)
	for v > 0 {
		n--
		b[n] = crockfordLower[v&0x1f]
		v >>= 5
	}
	if n == len(b) {
		return "0"
	}
	return string(b[n:])
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
