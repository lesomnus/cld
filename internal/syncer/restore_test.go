package syncer

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// collect reads a tar stream into a name->content map (regular files only)
// and a set of symlink names.
func collect(t *testing.T, r io.Reader) (map[string]string, map[string]string) {
	t.Helper()

	files := map[string]string{}
	links := map[string]string{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch hdr.Typeflag {
		case tar.TypeReg:
			data, err := io.ReadAll(tr)
			require.NoError(t, err)
			files[hdr.Name] = string(data)
		case tar.TypeSymlink:
			links[hdr.Name] = hdr.Linkname
		}
	}
	return files, links
}

func TestWriteBackup(t *testing.T) {
	l := Layout{
		GlobalDir:  filepath.Join(t.TempDir(), "global"),
		ProjectDir: filepath.Join(t.TempDir(), "project"),
	}
	require.NoError(t, os.MkdirAll(l.GlobalDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(l.ProjectDir, "projects", "-old"), 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(l.GlobalDir, ".credentials.json"), []byte("secret"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(l.ProjectDir, "projects", "-old", "s1.jsonl"),
		[]byte(`{"cwd":"/old/path","msg":"hi"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(l.ProjectDir, meta_name),
		[]byte(`{"workspace":"/old/path","encoded":"-old"}`), 0o644))

	meta := read_meta(l)
	require.Equal(t, "/old/path", meta.Workspace)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// New workspace differs → encoded dir renamed and cwd rewritten.
	require.NoError(t, write_backup(tw, l, ".claude", meta, "/new/path", "-new", 1000, 1000))
	require.NoError(t, tw.Close())

	files, _ := collect(t, &buf)

	t.Run("global files restored", func(t *testing.T) {
		require.Equal(t, "secret", files[".claude/.credentials.json"])
	})
	t.Run("meta file is not restored", func(t *testing.T) {
		_, ok := files[".claude/"+meta_name]
		require.False(t, ok)
	})
	t.Run("transcript dir renamed to the new encoding", func(t *testing.T) {
		content, ok := files[".claude/projects/-new/s1.jsonl"]
		require.True(t, ok)
		t.Run("cwd rewritten", func(t *testing.T) {
			require.Contains(t, content, "/new/path")
			require.NotContains(t, content, "/old/path")
		})
	})
}

func TestExtractSymlinkSafety(t *testing.T) {
	t.Run("rejects absolute symlink and does not follow it on write", func(t *testing.T) {
		root := t.TempDir()
		// A pre-existing symlink escaping the backup, as a prior sync might
		// have left before the fix.
		outside := filepath.Join(t.TempDir(), "victim")
		require.NoError(t, os.WriteFile(outside, []byte("original"), 0o644))

		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeSymlink, Name: "root/evil", Linkname: outside,
		}))
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: "root/evil", Mode: 0o644, Size: 7,
		}))
		tw.Write([]byte("payload"))
		require.NoError(t, tw.Close())

		err := extract(tar.NewReader(&buf), root)
		require.NoError(t, err)

		// The absolute symlink must not have been materialized...
		fi, err := os.Lstat(filepath.Join(root, "root", "evil"))
		require.NoError(t, err)
		require.Zero(t, fi.Mode()&os.ModeSymlink, "symlink should have been skipped")

		// ...and the outside victim must be untouched.
		got, err := os.ReadFile(outside)
		require.NoError(t, err)
		require.Equal(t, "original", string(got))
	})
}
