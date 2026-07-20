package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/lesomnus/cld/internal/tmuxx"
)

// install_dotfiles personalizes the session from the host user's ~/.dotfiles,
// mirroring VS Code Dev Containers' dotfiles feature: it copies the tree into
// the container's home and then, if it has an install.sh, runs it as the user
// with the copied dir as CWD; otherwise it symlinks the tree's top-level
// dotfiles into $HOME. The source is the host home the daemon sees through its
// read-only mount (config.DotfilesDir); an absent mount or an absent
// ~/.dotfiles is not an error. Best-effort and idempotent — it runs once per
// container generation (gated on e.dotfiles) and every failure is logged, never
// returned, so it cannot block provisioning.
func (d *Daemon) install_dotfiles(ctx context.Context, e *entry, id string) {
	if e.dotfiles || d.cfg.Dotfiles.Disabled || e.home == "" {
		return
	}
	src := d.cfg.DotfilesDir()
	if src == "" {
		return // the daemon has no host-home mount; nothing to read
	}
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		return // no ~/.dotfiles to install
	}

	// Clear any prior copy so a re-copy (e.g. after a failed attempt) is clean —
	// CopyDirToContainer overlays a tree rather than replacing it — then lay the
	// tree down owned by the container user.
	dir := path.Join(e.home, ".dotfiles")
	if err := d.remove_in_container(ctx, id, dir); err != nil {
		d.log.Warn("dotfiles: clear failed",
			slog.String("name", e.item.Name), slog.String("error", err.Error()))
		return
	}
	if err := dockerx.CopyDirToContainer(ctx, d.cli, id, e.home, ".dotfiles", src, e.uid, e.gid); err != nil {
		d.log.Warn("dotfiles: copy failed",
			slog.String("name", e.item.Name), slog.String("error", err.Error()))
		return
	}

	// os.Lstat (not Stat) so a *symlinked* install.sh — which CopyDirToContainer
	// drops — does not select the install branch and then fail on a missing file;
	// it falls through to symlinking instead.
	fi, err := os.Lstat(filepath.Join(src, "install.sh"))
	hasInstall := err == nil && fi.Mode().IsRegular()

	out, code, err := dockerx.ExecOutput(ctx, d.cli, id, e.user, []string{"sh", "-c", dotfilesScript(dir, e.home, hasInstall)})
	if err != nil {
		// Transport error — nothing ran; leave e.dotfiles unset so it retries.
		d.log.Warn("dotfiles: run failed",
			slog.String("name", e.item.Name), slog.String("error", err.Error()))
		return
	}
	// The script ran (whatever its exit): mark it done so a re-reconcile does not
	// re-run install.sh and re-apply side effects it made outside ~/.dotfiles.
	e.dotfiles = true
	if code != 0 {
		d.log.Warn("dotfiles: install script failed",
			slog.String("name", e.item.Name), slog.Int("code", code),
			slog.String("out", strings.TrimSpace(out)))
		return
	}
	d.log.Info("dotfiles installed",
		slog.String("id", short(id)), slog.String("name", e.item.Name))
}

// dotfilesScript builds the shell command run inside the container after
// ~/.dotfiles is copied to dir (an absolute container path), with home the
// container user's $HOME. When the tree has an install.sh (hasInstall) its CRLF
// line endings are normalized, it is made executable, and it is run with dir as
// CWD (honoring its shebang). Otherwise every top-level dotfile is symlinked
// into home with `ln -sfn` (idempotent), skipping ., .., and .git — the same
// fallback VS Code Dev Containers uses. An existing real directory at a target
// path is left untouched: `ln` into it would create a stray nested link, and
// clobbering it could drop container-provisioned content (e.g. ~/.local/bin).
func dotfilesScript(dir, home string, hasInstall bool) string {
	if hasInstall {
		// Strip CR so a Windows-authored install.sh does not fail with a
		// "bad interpreter: /bin/sh^M" on its shebang.
		return fmt.Sprintf("cd %s && tr -d '\\r' < install.sh > .install.sh.cld && "+
			"mv .install.sh.cld install.sh && chmod +x install.sh && HOME=%s ./install.sh",
			tmuxx.Quote(dir), tmuxx.Quote(home))
	}
	// `[ -e "$f" ]` skips the literal glob a POSIX shell leaves when nothing
	// matches; `${f##*/}` is the basename; rc surfaces any failed link.
	return fmt.Sprintf(`rc=0; for f in %s/.*; do [ -e "$f" ] || continue; n=${f##*/}; `+
		`case "$n" in .|..|.git) continue;; esac; t=%s/"$n"; `+
		`if [ -d "$t" ] && [ ! -L "$t" ]; then continue; fi; `+
		`ln -sfn "$f" "$t" || rc=1; done; exit $rc`,
		tmuxx.Quote(dir), tmuxx.Quote(home))
}
