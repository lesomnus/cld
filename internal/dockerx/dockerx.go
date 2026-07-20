// Package dockerx provides small helpers over the Docker API client:
// one-shot execs, file read/write via the archive endpoints, and path stats.
package dockerx

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// ExecOutput runs a command in a container and returns its combined stdout,
// its exit code, and any transport error. user may be empty for the
// container's default user.
func ExecOutput(ctx context.Context, cli *client.Client, ctr string, user string, cmd []string) (string, int, error) {
	created, err := cli.ExecCreate(ctx, ctr, client.ExecCreateOptions{
		User:         user,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	})
	if err != nil {
		return "", 0, err
	}

	att, err := cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", 0, err
	}
	defer att.Close()

	var out, errb bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &errb, att.Reader); err != nil {
		return "", 0, err
	}

	insp, err := cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return "", 0, err
	}
	if insp.ExitCode != 0 && out.Len() == 0 {
		out.Write(errb.Bytes())
	}
	return out.String(), insp.ExitCode, nil
}

// PathExists reports whether a path exists in the container filesystem.
func PathExists(ctx context.Context, cli *client.Client, ctr string, p string) (bool, error) {
	_, ok, err := FileSize(ctx, cli, ctr, p)
	return ok, err
}

// FileSize returns the size of a path in the container filesystem.
// ok is false without error when the path does not exist.
func FileSize(ctx context.Context, cli *client.Client, ctr string, p string) (int64, bool, error) {
	res, err := cli.ContainerStatPath(ctx, ctr, client.ContainerStatPathOptions{Path: p})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return res.Stat.Size, true, nil
}

// WriteFile writes a single file into the container via the archive endpoint.
// dir must exist; ownership and mode are carried in the tar header.
func WriteFile(ctx context.Context, cli *client.Client, ctr string, dir string, name string, mode int64, uid, gid int, data []byte) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     mode,
		Uid:      uid,
		Gid:      gid,
		Size:     int64(len(data)),
		ModTime:  time.Now(),
	})
	if err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}

	_, err = cli.CopyToContainer(ctx, ctr, client.CopyToContainerOptions{
		DestinationPath: dir,
		Content:         &buf,
	})
	return err
}

// CopyDirToContainer copies the host directory tree rooted at src into the
// container as destDir/name (destDir must exist). It emits explicit directory
// entries owned by uid:gid — unlike a bare docker cp of a nested file, which
// would leave the created parents owned by root. Only regular files and
// directories are copied.
func CopyDirToContainer(ctx context.Context, cli *client.Client, ctr, destDir, name, src string, uid, gid int) error {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			arc := name
			if rel != "." {
				arc = name + "/" + filepath.ToSlash(rel)
			}
			if d.IsDir() {
				return tw.WriteHeader(&tar.Header{
					Typeflag: tar.TypeDir, Name: arc + "/", Mode: 0o755, Uid: uid, Gid: gid,
				})
			}
			if !d.Type().IsRegular() {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			// Preserve the source's executable bit (as 0755) so dotfiles helper
			// scripts and ~/.local/bin executables stay runnable; everything else
			// is 0644.
			mode := int64(0o644)
			if info, err := d.Info(); err == nil && info.Mode()&0o111 != 0 {
				mode = 0o755
			}
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: arc, Mode: mode, Uid: uid, Gid: gid, Size: int64(len(data)),
			}); err != nil {
				return err
			}
			_, err = tw.Write(data)
			return err
		})
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()

	_, err := cli.CopyToContainer(ctx, ctr, client.CopyToContainerOptions{
		DestinationPath: destDir,
		Content:         pr,
	})
	return err
}

// CopyFileFromHost streams a host file into the container as dir/name.
func CopyFileFromHost(ctx context.Context, cli *client.Client, ctr string, dir string, name string, mode int64, src io.Reader, size int64) error {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     mode,
			Size:     size,
			ModTime:  time.Now(),
		})
		if err == nil {
			_, err = io.Copy(tw, src)
		}
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()

	_, err := cli.CopyToContainer(ctx, ctr, client.CopyToContainerOptions{
		DestinationPath: dir,
		Content:         pr,
	})
	return err
}

// ReadFile reads a single file from the container.
// Returns ok=false without error if the path does not exist.
func ReadFile(ctx context.Context, cli *client.Client, ctr string, p string) ([]byte, bool, error) {
	res, err := cli.CopyFromContainer(ctx, ctr, client.CopyFromContainerOptions{SourcePath: p})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer res.Content.Close()

	tr := tar.NewReader(res.Content)
	base := path.Base(p)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, err
		}
		if path.Base(hdr.Name) != base || hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, false, err
		}
		return data, true, nil
	}
	return nil, false, fmt.Errorf("no regular file %q in archive", p)
}
