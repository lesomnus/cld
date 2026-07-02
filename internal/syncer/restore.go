package syncer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/lesomnus/cld/internal/claude"
	"github.com/moby/moby/client"
)

// CopyIn restores the backup into the container's config dir, owned by
// uid:gid. When the workspace path changed since the backup was taken, the
// encoded transcript directory is renamed and cwd strings inside .jsonl
// transcripts are rewritten so resume keeps working.
func CopyIn(ctx context.Context, cli *client.Client, ctr string, cfg_dir string, l Layout, workspace string, uid, gid int) error {
	if !HasBackup(l) {
		return nil
	}

	meta := read_meta(l)
	new_enc := claude.EncodeProjectPath(workspace)

	home := path.Dir(cfg_dir)
	base := path.Base(cfg_dir)

	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := write_backup(tw, l, base, meta, workspace, new_enc, uid, gid)
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()

	_, err := cli.CopyToContainer(ctx, ctr, client.CopyToContainerOptions{
		DestinationPath: home,
		Content:         pr,
	})
	return err
}

func read_meta(l Layout) Meta {
	var m Meta
	data, err := os.ReadFile(filepath.Join(l.ProjectDir, meta_name))
	if err == nil {
		json.Unmarshal(data, &m)
	}
	return m
}

func write_backup(tw *tar.Writer, l Layout, base string, meta Meta, workspace string, new_enc string, uid, gid int) error {
	err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     base + "/",
		Mode:     0o700,
		Uid:      uid,
		Gid:      gid,
	})
	if err != nil {
		return err
	}

	err = add_tree_mapped(tw, l.GlobalDir, base, uid, gid, nil)
	if err != nil {
		return err
	}

	// Transcripts, with the encoded directory renamed if the workspace moved.
	old_enc := meta.Encoded
	if old_enc == "" {
		old_enc = new_enc
	}
	var rewrite func(rel string, data []byte) []byte
	if meta.Workspace != "" && meta.Workspace != workspace {
		rewrite = func(rel string, data []byte) []byte {
			if !strings.HasSuffix(rel, ".jsonl") {
				return data
			}
			return bytes.ReplaceAll(data, []byte(meta.Workspace), []byte(workspace))
		}
	}
	err = add_tree_mapped(tw, filepath.Join(l.ProjectDir, "projects", old_enc), base+"/projects/"+new_enc, uid, gid, rewrite)
	if err != nil {
		return err
	}
	return add_tree_mapped(tw, filepath.Join(l.ProjectDir, "file-history"), base+"/file-history", uid, gid, nil)
}

// add_tree_mapped writes the tree rooted at src under the archive path prefix.
// A missing src is a no-op.
func add_tree_mapped(tw *tar.Writer, src string, prefix string, uid, gid int, rewrite func(string, []byte) []byte) error {
	if _, err := os.Stat(src); err != nil {
		return nil
	}

	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.Name() == meta_name && path.Dir(rel) == "." {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return nil
		}
		name := prefix + "/" + rel

		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     name + "/",
				Mode:     0o755,
				Uid:      uid,
				Gid:      gid,
				ModTime:  fi.ModTime(),
			})

		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return nil
			}
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     name,
				Linkname: target,
				Mode:     0o777,
				Uid:      uid,
				Gid:      gid,
				ModTime:  fi.ModTime(),
			})

		case fi.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if rewrite != nil {
				data = rewrite(rel, data)
			}
			err = tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg,
				Name:     name,
				Mode:     int64(fi.Mode() & 0o777),
				Uid:      uid,
				Gid:      gid,
				Size:     int64(len(data)),
				ModTime:  fi.ModTime(),
			})
			if err != nil {
				return err
			}
			_, err = tw.Write(data)
			return err
		}
		return nil
	})
}
