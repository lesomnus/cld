package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/lesomnus/cld/internal/envx"
	"github.com/lesomnus/cld/internal/tmuxx"
)

// install_files places the host files cld.yaml declares into the container: the
// companion to session env for everything a variable cannot hold, such as the
// TLS material a remote Docker endpoint needs. The source is read through the
// same read-only host-home mount dotfiles uses, so nothing outside the user's
// home is reachable.
//
// Each spec carries a content hash into the container, so a placement is
// repeated only when what it would write actually changed — a rotating
// credential is re-copied, an unchanged one costs a hash. Best-effort like
// dotfiles: every failure is logged, never returned, so a missing source
// cannot block a session from coming up.
//
// This runs on the provisioning path, which means at container start, on a
// daemon restart, and whenever a container is re-provisioned — not on a timer.
// A credential that rotates while a session is up reaches it on the next one.
func (d *Daemon) install_files(ctx context.Context, e *entry, id string) {
	specs := d.file_specs(e)
	if len(specs) == 0 {
		return
	}

	made_marker_dir := false
	for _, f := range specs {
		src := d.cfg.HostPath(f.Src)
		if src == "" {
			d.log.Warn("files: unreadable source",
				slog.String("name", e.item.Name), slog.String("src", f.Src))
			continue
		}
		dst := d.expand_container_path(e, f.Dst)
		if !path.IsAbs(dst) {
			d.log.Warn("files: destination is not an absolute path",
				slog.String("name", e.item.Name), slog.String("dst", dst))
			continue
		}
		mode, err := f.Perm()
		if err != nil {
			d.log.Warn("files: "+err.Error(), slog.String("name", e.item.Name))
			continue
		}

		fi, err := os.Stat(src)
		if err != nil {
			d.log.Warn("files: source is missing",
				slog.String("name", e.item.Name), slog.String("src", f.Src),
				slog.String("error", err.Error()))
			continue
		}

		sum, err := hash_source(src, fi, mode)
		if err != nil {
			d.log.Warn("files: cannot read source",
				slog.String("name", e.item.Name), slog.String("src", f.Src),
				slog.String("error", err.Error()))
			continue
		}

		marker := path.Join(e.files_dir(), marker_name(dst))
		if cur, ok, err := dockerx.ReadFile(ctx, d.cli, id, marker); err == nil && ok && string(cur) == sum {
			continue // already placed, and unchanged since
		}

		if err := d.place_file(ctx, e, id, src, dst, fi, mode); err != nil {
			d.log.Warn("files: copy failed",
				slog.String("name", e.item.Name), slog.String("dst", dst),
				slog.String("error", err.Error()))
			continue
		}

		// Record only after a successful copy, so an interrupted one is retried.
		if !made_marker_dir {
			if err := d.mkdir_in_container(ctx, e, id, e.files_dir()); err != nil {
				d.log.Warn("files: cannot record placement",
					slog.String("name", e.item.Name), slog.String("error", err.Error()))
				continue
			}
			made_marker_dir = true
		}
		err = dockerx.WriteFile(ctx, d.cli, id, e.files_dir(), marker_name(dst), 0o600, e.uid, e.gid, []byte(sum))
		if err != nil {
			d.log.Warn("files: cannot record placement",
				slog.String("name", e.item.Name), slog.String("error", err.Error()))
			continue
		}
		d.log.Info("file placed",
			slog.String("id", short(id)), slog.String("name", e.item.Name),
			slog.String("dst", dst))
	}
}

// file_specs are the placements that apply to this container: the global ones
// first, then those of every matching project block, in file order.
func (d *Daemon) file_specs(e *entry) []config.FileSpec {
	out := append([]config.FileSpec{}, d.cfg.Files...)
	for _, p := range d.cfg.MatchProjects(e.item.LocalFolder) {
		out = append(out, p.Files...)
	}
	return out
}

// place_file copies one source into the container, creating its parent
// directory owned by the container user first — CopyToContainer would
// otherwise fail on a missing parent, or leave one owned by root.
func (d *Daemon) place_file(ctx context.Context, e *entry, id, src, dst string, fi os.FileInfo, mode int64) error {
	parent, name := path.Split(dst)
	parent = path.Clean(parent)
	if err := d.mkdir_in_container(ctx, e, id, parent); err != nil {
		return err
	}

	if fi.IsDir() {
		// Clear any previous copy: a tree is overlaid, not replaced, so a file
		// removed from the source would otherwise survive in the container.
		if err := d.remove_in_container(ctx, id, dst); err != nil {
			return err
		}
		return dockerx.CopyTreeToContainer(ctx, d.cli, id, parent, name, src, e.uid, e.gid, mode)
	}

	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return dockerx.CopyFileFromHostAs(ctx, d.cli, id, parent, name, mode, e.uid, e.gid, f, fi.Size())
}

// mkdir_in_container creates a directory owned by the container user.
func (d *Daemon) mkdir_in_container(ctx context.Context, e *entry, id, dir string) error {
	out, code, err := dockerx.ExecOutput(ctx, d.cli, id, e.user, []string{
		"sh", "-c", "mkdir -p " + tmuxx.Quote(dir),
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("mkdir %s: exit %d: %s", dir, code, out)
	}
	return nil
}

// expand_container_path resolves the ${HOME} and ${CLD_WORKSPACE} a config
// path may use. They are the two anchors a user cannot know in advance: the
// container's home depends on its remoteUser and the workspace on its mounts.
func (d *Daemon) expand_container_path(e *entry, p string) string {
	return envx.Expand(p, func(ns, name string) (string, bool) {
		if ns != "" {
			return "", false
		}
		switch name {
		case "HOME":
			return e.home, e.home != ""
		case "CLD_WORKSPACE":
			return e.item.Workspace, e.item.Workspace != ""
		}
		return "", false
	})
}

// files_dir holds one marker per placement, recording the hash of what was
// last written there. It lives in the container so it disappears with it —
// a recreated container is placed into afresh — and survives a daemon restart,
// which an in-memory flag would not.
func (e *entry) files_dir() string {
	return path.Join(e.cache_home, "cld", "files")
}

// marker_name keys a marker by its destination without letting that path
// escape the marker directory.
func marker_name(dst string) string {
	sum := sha256.Sum256([]byte(dst))
	return hex.EncodeToString(sum[:8]) + ".sha256"
}

// hash_source digests what would be written: the content, plus the mode and
// layout, so a permission or rename change is not mistaken for no change.
func hash_source(src string, fi os.FileInfo, mode int64) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "mode=%s\n", strconv.FormatInt(mode, 8))

	if !fi.IsDir() {
		f, err := os.Open(src)
		if err != nil {
			return "", err
		}
		defer f.Close()
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	// Sorted, so the digest does not depend on directory iteration order.
	var paths []string
	err := filepath.WalkDir(src, func(p string, entry os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if entry.Type().IsRegular() || entry.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)

	for _, p := range paths {
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "path=%s\n", filepath.ToSlash(rel))
		info, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
