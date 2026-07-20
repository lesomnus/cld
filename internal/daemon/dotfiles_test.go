package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDotfilesScript(t *testing.T) {
	t.Run("runs install.sh: normalizes CRLF, makes executable, honors shebang", func(t *testing.T) {
		s := dotfilesScript("/home/alice/.dotfiles", "/home/alice", true)
		require.Contains(t, s, `cd '/home/alice/.dotfiles'`)
		require.Contains(t, s, `tr -d '\r' < install.sh`)
		require.Contains(t, s, `chmod +x install.sh`)
		require.Contains(t, s, `HOME='/home/alice' ./install.sh`)
	})

	t.Run("symlinks top-level dotfiles when there is no install.sh", func(t *testing.T) {
		s := dotfilesScript("/home/alice/.dotfiles", "/home/alice", false)
		// Globs the copied dir's dotfiles, skips ./../.git, skips a real dir
		// target, aggregates failures into rc, idempotent ln -sfn.
		require.Contains(t, s, `for f in '/home/alice/.dotfiles'/.*`)
		require.Contains(t, s, `case "$n" in .|..|.git) continue;;`)
		require.Contains(t, s, `if [ -d "$t" ] && [ ! -L "$t" ]; then continue; fi`)
		require.Contains(t, s, `ln -sfn "$f" "$t" || rc=1`)
		require.Contains(t, s, `exit $rc`)
	})

	t.Run("shell-quotes paths with spaces", func(t *testing.T) {
		s := dotfilesScript("/home/a b/.dotfiles", "/home/a b", true)
		require.Contains(t, s, `cd '/home/a b/.dotfiles'`)
		require.Contains(t, s, `HOME='/home/a b'`)
	})
}

// TestDotfilesScriptExec runs the generated script through a real `sh` against
// temp dirs, so shell semantics (globbing, skips, chmod, idempotency) are
// exercised — the container path only differs by which shell runs it.
func TestDotfilesScriptExec(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	run := func(t *testing.T, script string) {
		t.Helper()
		out, err := exec.Command(sh, "-c", script).CombinedOutput()
		require.NoError(t, err, "script output: %s", out)
	}

	t.Run("symlink fallback links dotfiles, skips . .. and .git", func(t *testing.T) {
		dir := t.TempDir()
		home := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".bashrc"), []byte("x"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".vimrc"), []byte("y"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README"), []byte("z"), 0o644)) // not a dotfile
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))

		script := dotfilesScript(dir, home, false)
		run(t, script)
		run(t, script) // idempotent: ln -sfn must not error on the second pass

		for _, name := range []string{".bashrc", ".vimrc"} {
			target, err := os.Readlink(filepath.Join(home, name))
			require.NoError(t, err, "%s should be a symlink", name)
			require.Equal(t, filepath.Join(dir, name), target)
		}
		for _, name := range []string{"README", ".git"} {
			_, err := os.Lstat(filepath.Join(home, name))
			require.True(t, os.IsNotExist(err), "%s must not be linked into home", name)
		}
	})

	t.Run("symlink fallback replaces a real file but leaves a real dir untouched (no nested garbage)", func(t *testing.T) {
		dir := t.TempDir()
		home := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".bashrc"), []byte("mine"), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, ".config"), 0o755))
		// $HOME already has a real .bashrc file and a real .config dir (as
		// link_user_claude / a base image would leave).
		require.NoError(t, os.WriteFile(filepath.Join(home, ".bashrc"), []byte("theirs"), 0o644))
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".config"), 0o755))

		run(t, dotfilesScript(dir, home, false))

		// Real file is replaced by the dotfile symlink...
		target, err := os.Readlink(filepath.Join(home, ".bashrc"))
		require.NoError(t, err)
		require.Equal(t, filepath.Join(dir, ".bashrc"), target)
		// ...but the real directory is left as-is, and no ~/.config/.config
		// nested garbage link is created.
		fi, err := os.Lstat(filepath.Join(home, ".config"))
		require.NoError(t, err)
		require.True(t, fi.IsDir() && fi.Mode()&os.ModeSymlink == 0, ".config must stay a real dir")
		_, err = os.Lstat(filepath.Join(home, ".config", ".config"))
		require.True(t, os.IsNotExist(err), "must not create ~/.config/.config nested link")
	})

	t.Run("install.sh: CRLF-normalized, made executable, run with dir as CWD and HOME set", func(t *testing.T) {
		dir := t.TempDir()
		home := t.TempDir()
		// Non-executable (as a 0644 docker-cp copy would be), CRLF line endings
		// (Windows-authored), shebang-driven; writes proof of $HOME and $PWD.
		install := "#!/bin/sh\r\nprintf '%s\\n%s\\n' \"$HOME\" \"$PWD\" > \"$HOME/.installed\"\r\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "install.sh"), []byte(install), 0o644))

		run(t, dotfilesScript(dir, home, true))

		got, err := os.ReadFile(filepath.Join(home, ".installed"))
		require.NoError(t, err)
		require.Equal(t, home+"\n"+dir+"\n", string(got))
	})
}
