// Package syncer copies Claude Code state between containers and host-side
// backups. Backups are split: global/ holds project-independent state
// (credentials, settings) so a first-ever container of any project starts
// authenticated; projects/<key>/ holds transcripts and file history.
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

// Layout locates one container's backup on the host.
type Layout struct {
	GlobalDir  string
	ProjectDir string
}

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
	for _, d := range []string{l.GlobalDir, l.ProjectDir} {
		entries, err := os.ReadDir(d)
		if err == nil && len(entries) > 0 {
			return true
		}
	}
	return false
}

// CopyOut snapshots container state into the host backup.
// cfg_dir is the absolute path of the config dir inside the container.
func CopyOut(ctx context.Context, cli *client.Client, ctr string, cfg_dir string, l Layout, workspace string, global, project bool) error {
	if project {
		if err := copy_out_project(ctx, cli, ctr, cfg_dir, l, workspace); err != nil {
			return err
		}
	}
	if global {
		if err := copy_out_global(ctx, cli, ctr, cfg_dir, l); err != nil {
			return err
		}
	}
	return nil
}

func copy_out_project(ctx context.Context, cli *client.Client, ctr string, cfg_dir string, l Layout, workspace string) error {
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

// copy_out_global copies only the global top-level entries of the config dir,
// fetching each one individually so the (potentially huge) projects/ and
// file-history/ trees are never streamed out just to be discarded.
func copy_out_global(ctx context.Context, cli *client.Client, ctr string, cfg_dir string, l Layout) error {
	names, err := list_dir(ctx, cli, ctr, cfg_dir)
	if err != nil {
		return err
	}

	for _, name := range names {
		if claude.Classify(name) != claude.BackupGlobal {
			continue
		}
		if err := fetch(ctx, cli, ctr, path.Join(cfg_dir, name), l.GlobalDir); err != nil {
			return err
		}
	}
	return sanitize_global_state(l.GlobalDir)
}

// sanitize_global_state reduces the freshly-fetched .claude.json in the shared
// global backup to its project-independent keys, so per-project state (keyed by
// the identical in-container workspace path across every devcontainer) never
// bleeds between projects on restore. A file that cannot be parsed is dropped
// rather than stored intact — keeping it would leak the very projects map this
// strips. A missing file is a no-op.
func sanitize_global_state(dir string) error {
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
// ProjectDir/projects/<enc>/..., and a single file ".credentials.json"
// fetched into GlobalDir lands at GlobalDir/.credentials.json. A missing
// source is not an error.
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
