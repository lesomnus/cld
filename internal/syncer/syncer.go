// Package syncer copies Claude Code state between containers and host-side
// backups. A backup is keyed by project (see Daemon.backup_key) and holds two
// kinds of state: settings/ has the project-independent-looking files
// (settings.json, .claude.json, CLAUDE.md, agents/, commands/, skills/,
// output-styles/, plugins/) and projects/+file-history/ have the transcripts.
// Both live under the SAME per-project dir, never a bucket shared across
// projects, so a change made inside one project's container can never bleed
// into another project's container on restore.
package syncer

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/lesomnus/cld/internal/claude"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/lesomnus/cld/internal/tmuxx"
	"github.com/moby/moby/client"
)

// Layout locates one container's isolated, per-project backup on the host.
type Layout struct {
	ProjectDir string
}

// settingsDir is the ProjectDir subdirectory holding the project's own copy
// of the project-independent-looking Claude Code files (see package doc).
// It is a subdirectory (rather than ProjectDir's root) so it cannot collide
// with the "projects", "file-history", and meta_name entries also stored
// there.
const settingsDir = "settings"

// Meta records what the project backup was taken from, so a restore into a
// container with a different workspace path can migrate the encoded
// directory name and the cwd strings inside transcripts.
type Meta struct {
	Workspace string `json:"workspace"`
	Encoded   string `json:"encoded"`
}

const meta_name = "cld-meta.json"

// HasBackup reports whether there is anything to restore.
func HasBackup(l Layout) bool {
	entries, err := os.ReadDir(l.ProjectDir)
	return err == nil && len(entries) > 0
}

// CopyOut snapshots container state into the host's per-project backup dir.
// cfg_dir is the absolute path of the config dir inside the container.
// settings selects the project-independent-looking files; transcripts
// selects the per-conversation state. Both are written under the same
// Layout.ProjectDir — see the package doc for why.
func CopyOut(ctx context.Context, cli *client.Client, ctr string, cfg_dir string, l Layout, workspace string, settings, transcripts bool) error {
	if transcripts {
		if err := copy_out_transcripts(ctx, cli, ctr, cfg_dir, l, workspace); err != nil {
			return err
		}
	}
	if settings {
		if err := copy_out_settings(ctx, cli, ctr, cfg_dir, l); err != nil {
			return err
		}
	}
	return nil
}

func copy_out_transcripts(ctx context.Context, cli *client.Client, ctr string, cfg_dir string, l Layout, workspace string) error {
	enc := claude.EncodeProjectPath(workspace)

	// fetch roots entries at their container basename, so "projects/<enc>" is
	// fetched under ProjectDir/projects and "file-history" under ProjectDir.
	err := fetch(ctx, cli, ctr, path.Join(cfg_dir, "projects", enc), filepath.Join(l.ProjectDir, "projects"))
	if err != nil {
		return err
	}
	err = fetch(ctx, cli, ctr, path.Join(cfg_dir, "file-history"), l.ProjectDir)
	if err != nil {
		return err
	}

	meta, err := json.Marshal(Meta{Workspace: workspace, Encoded: enc})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.ProjectDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(l.ProjectDir, meta_name), meta, 0o644)
}

// copy_out_settings copies only the BackupSettings-classified top-level
// entries of the config dir into this project's own settingsDir, fetching
// each one individually so the (potentially huge) projects/ and
// file-history/ trees are never streamed out just to be discarded.
func copy_out_settings(ctx context.Context, cli *client.Client, ctr string, cfg_dir string, l Layout) error {
	names, err := list_dir(ctx, cli, ctr, cfg_dir)
	if err != nil {
		return err
	}

	dst := filepath.Join(l.ProjectDir, settingsDir)
	for _, name := range names {
		if claude.Classify(name) != claude.BackupSettings {
			continue
		}
		if err := fetch(ctx, cli, ctr, path.Join(cfg_dir, name), dst); err != nil {
			return err
		}
	}
	return sanitize_settings_state(dst)
}

// sanitize_settings_state reduces the freshly-fetched .claude.json in this
// project's settingsDir to its project-independent keys, dropping the
// per-project "projects" map (which claude itself keys by the in-container
// workspace path, e.g. "/workspace", regardless of the host-side project). It
// is otherwise redundant with the transcripts already stored alongside it, and
// stripping keeps this file's job narrow: the account/UI-level settings this
// project's container had, not a second copy of its conversation state. A
// file that cannot be parsed is dropped rather than stored intact — keeping
// it would leak the very projects map this strips. A missing file is a no-op.
func sanitize_settings_state(dir string) error {
	p := filepath.Join(dir, ".claude.json")
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	stripped, ok := claude.StripProjectState(data)
	if !ok {
		return os.Remove(p)
	}
	return os.WriteFile(p, stripped, 0o600)
}

// list_dir lists the immediate entries of a container directory, excluding
// "." and "..". A missing directory yields no names.
func list_dir(ctx context.Context, cli *client.Client, ctr string, dir string) ([]string, error) {
	out, code, err := dockerx.ExecOutput(ctx, cli, ctr, "", []string{
		"sh", "-c", "ls -1a " + tmuxx.Quote(dir) + " 2>/dev/null",
	})
	if err != nil {
		return nil, err
	}
	if code != 0 {
		// Directory absent or unreadable; nothing to copy.
		return nil, nil
	}

	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "." || line == ".." {
			continue
		}
		names = append(names, line)
	}
	return names, nil
}

// fetch copies a container path out and extracts it under dst_root, rooted at
// the source's basename (as the Docker archive endpoint names it). So a
// directory "projects/<enc>" fetched into ProjectDir/projects lands at
// ProjectDir/projects/<enc>/..., and a single file "settings.json" fetched
// into ProjectDir/settings lands at ProjectDir/settings/settings.json. A
// missing source is not an error.
func fetch(ctx context.Context, cli *client.Client, ctr string, src string, dst_root string) error {
	res, err := cli.CopyFromContainer(ctx, ctr, client.CopyFromContainerOptions{SourcePath: src})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	defer res.Content.Close()

	return extract(tar.NewReader(res.Content), dst_root)
}

// extract writes tar entries under dst_root, preserving their archive paths.
func extract(tr *tar.Reader, dst_root string) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		rel := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if rel == "." || !fs.ValidPath(rel) {
			continue
		}
		p := filepath.Join(dst_root, filepath.FromSlash(rel))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Reject container-controlled symlinks that point outside the
			// backup: materializing them lets later writes escape the backup
			// dir (e.g. onto host dotfiles) via a stale link.
			if !symlink_is_safe(hdr.Linkname) {
				continue
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.Remove(p)
			if err := os.Symlink(hdr.Linkname, p); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			// Remove any pre-existing entry (possibly a symlink) so the open
			// cannot follow a stale link onto an unrelated host file.
			os.Remove(p)
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|os.O_EXCL, fs.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if err2 := f.Close(); err == nil {
				err = err2
			}
			if err != nil {
				return err
			}
			os.Chtimes(p, hdr.ModTime, hdr.ModTime)
		}
	}
}

// symlink_is_safe reports whether a container-supplied symlink target is safe
// to materialize in the backup: it must be relative and must not climb out of
// its directory with "..". This keeps a stale link from later redirecting a
// write onto an unrelated host file.
func symlink_is_safe(target string) bool {
	if filepath.IsAbs(target) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(target), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}
