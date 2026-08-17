package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"

	"github.com/lesomnus/cld/internal/claude"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/lesomnus/cld/internal/envx"
	"github.com/lesomnus/cld/internal/ghcli"
	"github.com/lesomnus/cld/internal/release"
	"github.com/lesomnus/cld/internal/syncer"
	"github.com/lesomnus/cld/internal/tmuxx"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type container_inspect = container.InspectResponse

const install_dir = "/usr/local/bin"

// ensure is idempotent and safe to re-run on every event: it resolves the
// container's identity, installs the binaries, restores and seeds state,
// and starts the session and the watcher — each step only if missing.
// Session creation happens once per container generation (session_done,
// keyed on StartedAt) so a session the user closed is not resurrected.
// It runs on the container's worker goroutine, so entry state needs no lock.
func (d *Daemon) ensure(ctx context.Context, e *entry) {
	err := d.ensure_(ctx, e)
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}

	e.item.Status = StatusFailed
	e.item.Error = err.Error()
	e.publish()
	d.log.Error("provision failed", slog.String("id", short(e.id)), slog.String("error", err.Error()))
}

func (d *Daemon) ensure_(ctx context.Context, e *entry) error {
	id := e.id
	insp, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	c := insp.Container

	labels := map[string]string{}
	if c.Config != nil {
		labels = c.Config.Labels
	}
	local_folder := labels[devc.LabelLocalFolder]
	if local_folder == "" || devc.Ignored(labels, local_folder, d.cfg.Ignore) {
		d.remove(e)
		return nil
	}

	// A stopped container cannot be exec'd into, so it cannot be provisioned;
	// keep it visible in the listing as stopped with what its labels tell us.
	if c.State == nil || !c.State.Running {
		d.mark_stopped(e, labels, local_folder)
		return nil
	}

	if e.item.LocalFolder == "" {
		e.item.LocalFolder = local_folder
	}

	// A new container generation (restart) re-opens the session decision.
	if c.State.StartedAt != e.started_at {
		e.started_at = c.State.StartedAt
		e.session_done = false
		e.session_failed = false
	}

	// A container that already reached ready and has its session should not
	// pay the full provisioning cost on every reconcile; a user-ended
	// session must keep its status too, and a session that exited non-zero
	// must stay visible as failed instead of being silently flipped back to
	// ready on the next reconcile (it is retried by `cld it --new` or a
	// container restart).
	settled := e.item.Status == StatusReady || e.item.Status == StatusSessionEnded ||
		(e.item.Status == StatusFailed && e.session_failed)
	if settled && e.cfg_dir != "" && e.session_done && e.watch_stop != nil {
		return nil
	}

	if e.item.Status != StatusSessionEnded {
		e.item.Status = StatusProvisioning
	}
	e.item.Error = ""
	e.publish()

	if e.cfg_dir == "" {
		if err := d.resolve(ctx, e, id, labels, &c); err != nil {
			return fmt.Errorf("resolve identity: %w", err)
		}
	}

	if e.item.Name == "" {
		// Name is the stable managed identity (it feeds the tmux session name),
		// so it keeps the FULL devcontainer.json "name" — a namespaced
		// "lesomnus/cld" stays "lesomnus-cld" so an already-running session keeps
		// matching across upgrades. Display collapses it to the last segment for
		// readability. Both fall back to the folder name.
		name := devc.Slug(e.dev_name)
		if name == "" {
			name = devc.Slug(devc.DisplayName(local_folder))
		}
		if name == "" {
			name = "devcontainer"
		}
		display := devc.Slug(devc.BaseName(e.dev_name))
		if display == "" {
			display = name
		}
		e.item.Name = d.unique_name(id, name)
		e.item.Display = display
		// A short handle for the container, derived from the FULL name (not the
		// collapsed display) so a namespaced "lesomnus/cld" yields the segment
		// initials "lc" rather than "cld"; kept unique across the fleet by
		// appending a digest of the workspace path.
		e.item.Alias = d.unique_alias(id, devc.Alias(name), local_folder)
		e.publish()
	}

	version, err := d.install_claude(ctx, e, id, "")
	if err != nil {
		return fmt.Errorf("install claude: %w", err)
	}
	e.version = version
	e.item.Version = version

	if e.arch_ok {
		if err := d.install_self(ctx, id); err != nil {
			return fmt.Errorf("install cld: %w", err)
		}
	}

	// Best-effort: when the workspace has a GitHub remote, inject the gh CLI so
	// it is on PATH in the container. Independent of the arch match — gh is
	// fetched for the container's own architecture — and a convenience, so a
	// failure must not block provisioning.
	d.install_gh(ctx, e, id)

	if err := d.prepare_state(ctx, e, id); err != nil {
		return fmt.Errorf("prepare state: %w", err)
	}

	// Best-effort: personalize the session from the host's ~/.dotfiles (run its
	// install.sh, or symlink it), like VS Code Dev Containers. Shell-only, so not
	// arch-gated. After prepare_state, which mkdir's the user's $HOME the copy
	// targets.
	d.install_dotfiles(ctx, e, id)

	// Best-effort: give VS Code / Cursor a "claude" terminal profile that runs
	// `cld it`. Needs the in-container cld binary (arch match).
	if e.arch_ok {
		d.install_vscode_profile(ctx, e, id)
	}

	// Best-effort: place the host files cld.yaml declares for this workspace —
	// what session env cannot carry, such as a remote engine's TLS material.
	// After dotfiles, so an explicit placement wins over a dotfile of the same
	// name, and before the session, so claude sees it from its first prompt.
	d.install_files(ctx, e, id)

	// Best-effort: bring up this project's own Docker engine and attach the
	// container to it, when the config asks for one. Before the scripts, so a
	// setup script can already use it, and before the session, so DOCKER_HOST
	// is part of the environment claude starts with.
	d.ensure_dind(ctx, e, id)

	// The user's own scripts, last of the provisioning steps: everything cld
	// installs is in place, so a script can build on it, and the session does
	// not exist yet, so what a script installs is there from claude's first
	// prompt. Only a script marked `on_error: fail` stops provisioning.
	for _, ev := range []scriptEvent{scriptSetup, scriptStart} {
		if err := d.run_scripts(ctx, e, id, ev); err != nil {
			return fmt.Errorf("%s script: %w", ev, err)
		}
	}

	if !e.session_done {
		// Suppress recreation of a session the user ended in this generation,
		// even across a daemon restart (the record is on disk).
		st := d.sessions.get(id)
		if st.Ended && st.Gen == e.started_at {
			e.item.Status = StatusSessionEnded
		} else {
			if err := d.ensure_session(ctx, e, id); err != nil {
				return fmt.Errorf("session: %w", err)
			}
			// A live session exists again (e.g. after a container restart of a
			// previously ended container), so clear a stale session-ended
			// status; the promotion below sets it to ready.
			if e.item.Status == StatusSessionEnded {
				e.item.Status = StatusProvisioning
			}
		}
		e.session_done = true
	}

	if e.watch_stop == nil {
		wctx, stop := context.WithCancel(d.base_ctx)
		e.watch_stop = stop
		go d.sync_loop(wctx, e)
		if e.arch_ok {
			go d.watch_container(wctx, e, id)
			if d.cfg.Auth.ForwardAgentEnabled() {
				go d.relay_agent(wctx, e, id)
			}
			// Expose the daemon's control API inside the container so `cld it`
			// run there can reach and attach to this session.
			go d.relay_api(wctx, e, id)
			// Expose the auth proxy so a broker session authenticates through it
			// (no-op when the broker is inactive).
			go d.relay_proxy(wctx, e, id)
			// Receive claude's own consumption metrics (no-op when telemetry is
			// disabled). Cross-arch containers get none: like every other relay
			// this needs cld's binary to run in the container.
			go d.relay_otlp(wctx, e, id)
		} else {
			go d.poll_container(wctx, e)
		}
		// The scoped control API is reachable in-container exactly when the arch
		// matches (the cld binary is installed) and remote control is enabled
		// (relay_api actually serves), so claude's hooks can push live activity
		// over it. When they can, the worker owns e.item.Activity and the listing
		// trusts the snapshot instead of scraping the tmux pane. Reassigned every
		// time this block runs (a container restart clears watch_stop and re-runs
		// it) so it can never go stale. See fillActivity.
		e.activity_pushed = e.arch_ok && d.cfg.Auth.RemoteControlEnabled()
	}

	if e.item.Status == StatusProvisioning {
		e.item.Status = StatusReady
	}
	// Seed the conversation title from any resumed transcript so a listing shows
	// it immediately, without waiting for the first transcript change to sync.
	d.refresh_title(ctx, e)
	// Likewise seed workflow-run state from a resumed session's journals.
	d.refresh_workflows(ctx, e)
	// Seed the initial conversation activity before claude's first hook fires, so
	// a just-ready push container is never blank: idle with no conversation yet,
	// waiting once a resumed transcript gave it a title. Non-push containers keep
	// Activity empty and are classified from the pane at listing time.
	if e.activity_pushed && e.item.Status == StatusReady && e.item.Activity == "" {
		e.item.Activity = classifyActivity("", e.item.Title)
	}
	e.publish()
	d.log.Info("ready",
		slog.String("id", short(id)),
		slog.String("name", e.item.Name),
		slog.String("version", version))
	return nil
}

// mark_stopped keeps a container that is not running in the listing as stopped.
// It cannot be exec'd into, so its identity is resolved from labels alone —
// enough for `cld ls` to show a name and folder. An entry the daemon already
// provisioned while running keeps everything it resolved; this only fills the
// gaps for a container first seen stopped, e.g. after a daemon restart. A
// session the user ended keeps that status, matching stop.
func (d *Daemon) mark_stopped(e *entry, labels map[string]string, local_folder string) {
	if e.item.LocalFolder == "" {
		e.item.LocalFolder = local_folder
	}
	if e.item.Name == "" {
		var config_file []byte
		if p := labels[devc.LabelConfigFile]; p != "" {
			config_file, _ = os.ReadFile(p)
		}
		// Name keeps the full devcontainer.json "name" (stable identity); Display
		// collapses it to the last segment for readability. Both fall back to the
		// folder name. See ensure_ for why the two are kept separate.
		project := devc.ProjectName(config_file)
		name := devc.Slug(project)
		if name == "" {
			name = devc.Slug(devc.DisplayName(local_folder))
		}
		if name == "" {
			name = "devcontainer"
		}
		display := devc.Slug(devc.BaseName(project))
		if display == "" {
			display = name
		}
		e.item.Name = d.unique_name(e.id, name)
		e.item.Display = display
		// Alias from the FULL name (segment initials, e.g. "lc"), not the
		// collapsed display. See ensure_.
		e.item.Alias = d.unique_alias(e.id, devc.Alias(name), local_folder)
	}
	if e.item.Status != StatusSessionEnded {
		e.item.Status = StatusStopped
	}
	e.item.Error = ""
	e.publish()
}

// stop handles a die event: the container stopped but may start again. Tear
// down the session and watcher and take a final backup, but keep the entry so
// a restart is recognized.
func (d *Daemon) stop(ctx context.Context, e *entry) {
	if e.watch_stop != nil {
		e.watch_stop()
		e.watch_stop = nil
	}
	if e.item.Workspace != "" {
		d.copy_out(ctx, e, dirty{settings: true, transcript: true})
	}
	if e.item.Name != "" {
		d.tmux.KillSession(ctx, devc.SessionName(e.item.Name))
	}
	e.session_done = false
	if e.item.Status != StatusSessionEnded {
		e.item.Status = StatusStopped
	}
	// Drop the last pushed conversation activity: it belonged to the session
	// that just died. Clearing it both blanks the listing for a non-ready
	// container and lets ensure_'s seed (guarded on Activity=="") re-fire when
	// the container restarts, instead of the next generation inheriting a stale
	// "working"/"waiting" that only a completed turn's hook would correct.
	e.item.Activity = ""
	// Likewise drop the workflow runs — they belong to the dead session and
	// none are live now; a restart re-seeds them from disk. (copy_out above may
	// have just refreshed them, so clear after it.)
	e.item.Workflows = nil
	e.publish()
	d.log.Info("stopped", slog.String("id", short(e.id)), slog.String("name", e.item.Name))
}

// teardown handles a destroy (or a container that vanished from the listing):
// the container is gone for good, so finalize and drop the entry.
func (d *Daemon) teardown(ctx context.Context, e *entry) {
	d.stop(ctx, e)
	// The devcontainer is gone for good, so its engine has nothing left to
	// serve. This is the path a plain `docker rm` takes, where nothing else
	// would ever clean it up.
	d.remove_dind(ctx, e)
	d.sessions.clear(e.id)
	d.remove(e)
	d.log.Info("retired", slog.String("id", short(e.id)), slog.String("name", e.item.Name))
}

// resolve figures out the effective user, its home, the config dir, the
// workspace path, the platform, and whether the config dir is bind-mounted.
func (d *Daemon) resolve(ctx context.Context, e *entry, id string, labels map[string]string, c *container_inspect) error {
	// Prefer the devcontainer's remoteUser, then the image's own USER, and
	// otherwise the container's default user (empty = whatever `docker exec`
	// runs as) rather than guessing a uid that may not exist in the image.
	user := devc.RemoteUser(labels[devc.LabelMetadata])
	if user == "" && c.Config != nil {
		user = c.Config.User
	}
	if c.Config != nil {
		// What every exec into this container inherits; the base the session
		// environment is resolved over.
		e.container_env = c.Config.Env
	}

	// One probe yields uid, gid, home, the cache dir, and libc, each on its own
	// line, as the target user (the musl check works regardless of user). The
	// cache dir mirrors Go's os.UserCacheDir so the relay socket lands where an
	// in-container `cld` looks for the daemon.
	out, code, err := dockerx.ExecOutput(ctx, d.cli, id, user, []string{
		"sh", "-c", `id -u; id -g; printf '%s\n' "$HOME"; printf '%s\n' "${XDG_CACHE_HOME:-$HOME/.cache}"; ` +
			`if [ -e /lib/ld-musl-x86_64.so.1 ] || [ -e /lib/ld-musl-aarch64.so.1 ]; then echo musl; else echo gnu; fi`,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("probe user %q: exit %d: %s", user, code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 5 {
		return fmt.Errorf("probe user %q: unexpected output %q", user, out)
	}
	e.uid, _ = strconv.Atoi(strings.TrimSpace(lines[0]))
	e.gid, _ = strconv.Atoi(strings.TrimSpace(lines[1]))
	e.home = lines[2]
	if e.home == "" || e.home == "/" {
		return fmt.Errorf("user %q has no usable home", user)
	}
	e.cache_home = lines[3]
	if e.cache_home == "" {
		e.cache_home = e.home + "/.cache"
	}
	// Pin the resolved uid so every later exec targets the same user even when
	// the default user was used (empty string) at probe time.
	e.user = user
	if e.user == "" {
		e.user = strings.TrimSpace(lines[0])
	}
	e.cfg_dir = claude.ConfigDirIn(e.home)
	musl := lines[4] == "musl"

	mounts := make([]devc.Mount, 0, len(c.Mounts))
	for _, m := range c.Mounts {
		mounts = append(mounts, devc.Mount{Source: m.Source, Destination: m.Destination})
	}

	config_file := d.read_config_file(ctx, id, labels[devc.LabelConfigFile], mounts)
	e.dev_name = devc.ProjectName(config_file)
	e.remote_env = devc.RemoteEnv(labels[devc.LabelMetadata], config_file)
	e.item.Workspace = devc.WorkspaceFolder(config_file, e.item.LocalFolder, mounts)
	if e.item.Workspace == "" {
		return fmt.Errorf("cannot determine workspace folder for %s", e.item.LocalFolder)
	}

	arch := ""
	if img, err := d.cli.ImageInspect(ctx, c.Image); err == nil {
		arch = img.Architecture
	}
	if arch == "" {
		arch = runtime.GOARCH
	}

	platform, err := release.PlatformFor(arch, musl)
	if err != nil {
		return err
	}
	e.platform = platform
	e.arch = arch
	e.arch_ok = arch == runtime.GOARCH
	return nil
}

// install_claude puts a version into the container as claude-<version> and
// atomically points the "claude" symlink at it, so a running binary is never
// overwritten (ETXTBSY) and live sessions keep their version. channel selects
// the release channel to install: "" is the daemon's tracked channel (the normal
// provisioning path), a non-empty channel (e.g. "latest") is resolved fresh for
// a one-off `cld update --channel`. The copy verifies the installed size to
// detect an interrupted earlier copy, and re-fetches the host binary if the
// cache was garbage-collected out from under it.
func (d *Daemon) install_claude(ctx context.Context, e *entry, id string, channel string) (string, error) {
	version, bin, err := d.rel.EnsureChannel(ctx, channel, e.platform)
	if err != nil {
		return "", err
	}

	name := "claude-" + version
	if err := d.install_binary(ctx, id, bin, name); err != nil {
		// The cache may have been GC'd between Ensure and open; retry once.
		if os.IsNotExist(err) {
			if _, bin, err = d.rel.EnsureChannel(ctx, channel, e.platform); err != nil {
				return "", err
			}
			if err = d.install_binary(ctx, id, bin, name); err != nil {
				return "", err
			}
		} else {
			return "", err
		}
	}

	out, code, err := dockerx.ExecOutput(ctx, d.cli, id, "0", []string{
		"ln", "-sfn", name, path.Join(install_dir, "claude"),
	})
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("symlink: exit %d: %s", code, out)
	}

	d.link_user_claude(ctx, e, id)
	return version, nil
}

// link_user_claude points the container user's ~/.local/bin/claude at the
// installed binary. Claude Code checks that default install path for
// self-management and warns ("claude command … missing or broken · run claude
// install to repair") when it is absent — cld installs to /usr/local/bin (on
// PATH) instead. The link targets install_dir/claude, not the versioned name,
// so it follows version bumps. Best-effort and cosmetic: the /usr/local/bin
// binary works regardless, so a failure must not block provisioning.
func (d *Daemon) link_user_claude(ctx context.Context, e *entry, id string) {
	if e.home == "" {
		return
	}
	link := path.Join(e.home, ".local", "bin", "claude")
	target := path.Join(install_dir, "claude")
	out, code, err := dockerx.ExecOutput(ctx, d.cli, id, e.user, []string{
		"sh", "-c", fmt.Sprintf("mkdir -p %s && ln -sfn %s %s",
			tmuxx.Quote(path.Dir(link)), tmuxx.Quote(target), tmuxx.Quote(link)),
	})
	if err != nil {
		d.log.Warn("link ~/.local/bin/claude failed",
			slog.String("name", e.item.Name), slog.String("error", err.Error()))
	} else if code != 0 {
		d.log.Warn("link ~/.local/bin/claude failed",
			slog.String("name", e.item.Name), slog.String("out", strings.TrimSpace(out)))
	}
}

// install_self copies the cld executable into the container for use as the
// in-container watcher. Only when the architectures match; cld is a static
// binary so libc does not matter.
func (d *Daemon) install_self(ctx context.Context, id string) error {
	return d.install_binary(ctx, id, d.self, "cld")
}

// install_gh injects the GitHub CLI into the container at install_dir/gh when
// the workspace is a git repository with a GitHub remote. gh is fetched for the
// container's architecture (libc-agnostic static binary) and installed the same
// way as claude. Entirely best-effort: it is disabled by config for users who
// don't want it, skipped when there is no GitHub remote, and every failure is
// logged, never returned, so it can't fail provisioning.
func (d *Daemon) install_gh(ctx context.Context, e *entry, id string) {
	if d.cfg.Gh.Disabled {
		return
	}
	if !d.has_github_remote(ctx, e, id) {
		return
	}

	arch, err := ghcli.ArchFor(e.arch)
	if err != nil {
		d.log.Warn("skip gh install",
			slog.String("name", e.item.Name), slog.String("error", err.Error()))
		return
	}

	version, bin, err := d.gh.Ensure(ctx, arch)
	if err != nil {
		d.log.Warn("fetch gh failed",
			slog.String("name", e.item.Name), slog.String("error", err.Error()))
		return
	}
	if err := d.install_binary(ctx, id, bin, "gh"); err != nil {
		d.log.Warn("install gh failed",
			slog.String("name", e.item.Name), slog.String("error", err.Error()))
		return
	}
	d.log.Info("gh installed",
		slog.String("id", short(id)), slog.String("name", e.item.Name),
		slog.String("version", version))
}

// has_github_remote reports whether the container's workspace is a git
// repository with a remote pointing at github.com. It reads every configured
// remote URL (https and ssh both contain the host), so any GitHub remote —
// origin or otherwise — counts. A non-repo, a repo with no GitHub remote, or a
// container without git all read as false.
func (d *Daemon) has_github_remote(ctx context.Context, e *entry, id string) bool {
	if e.item.Workspace == "" {
		return false
	}
	out, code, err := dockerx.ExecOutput(ctx, d.cli, id, e.user, []string{
		"git", "-C", e.item.Workspace, "config", "--get-regexp", `^remote\..*\.url$`,
	})
	if err != nil || code != 0 {
		return false
	}
	return strings.Contains(out, "github.com")
}

// install_binary copies a host binary into install_dir/name unless a
// correctly-sized copy is already there. It writes to a temp name and
// renames into place inside the container, so an interrupted copy never
// leaves a truncated binary at the final path, then verifies the installed
// bytes against the host file's sha256 (best effort; skipped if the container
// lacks sha256sum).
func (d *Daemon) install_binary(ctx context.Context, id string, src string, name string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}

	dst := path.Join(install_dir, name)
	if size, ok, err := dockerx.FileSize(ctx, d.cli, id, dst); err != nil {
		return err
	} else if ok && size == fi.Size() {
		return nil
	}

	sum, err := sha256_file(f)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	tmp := ".cld-" + name + ".tmp"
	if err := dockerx.CopyFileFromHost(ctx, d.cli, id, install_dir, tmp, 0o755, f, fi.Size()); err != nil {
		return err
	}
	out, code, err := dockerx.ExecOutput(ctx, d.cli, id, "0", []string{
		"mv", "-f", path.Join(install_dir, tmp), dst,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("install %s: exit %d: %s", name, code, out)
	}

	// Verify the installed bytes match the source; catches copy corruption.
	got, code, err := dockerx.ExecOutput(ctx, d.cli, id, "0", []string{
		"sh", "-c", "sha256sum " + tmuxx.Quote(dst) + " 2>/dev/null | cut -d' ' -f1",
	})
	if err != nil {
		return err
	}
	got = strings.TrimSpace(got)
	if code == 0 && got != "" && got != sum {
		return fmt.Errorf("checksum mismatch after installing %s: got %s want %s", name, got, sum)
	}
	return nil
}

func sha256_file(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// prepare_state restores the backup into a fresh container and seeds the
// onboarding, trust, and retention keys. Restore happens once per container
// generation and only when the container has no state of its own yet —
// a restarted container's state is newer than any backup.
func (d *Daemon) prepare_state(ctx context.Context, e *entry, id string) error {
	// The config dir is <home>/.cld/claude; create and own both levels.
	parent := path.Dir(e.cfg_dir)
	out, code, err := dockerx.ExecOutput(ctx, d.cli, id, "0", []string{
		"sh", "-c", fmt.Sprintf(`mkdir -p %s && chown %d:%d %s %s`,
			tmuxx.Quote(e.cfg_dir), e.uid, e.gid, tmuxx.Quote(parent), tmuxx.Quote(e.cfg_dir)),
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("create config dir: exit %d: %s", code, out)
	}

	state_path := path.Join(e.cfg_dir, ".claude.json")
	if !e.restored {
		_, ok, err := dockerx.ReadFile(ctx, d.cli, id, state_path)
		if err != nil {
			return err
		}
		if !ok {
			l := d.layout(e)
			pl := d.proj_locks.get(d.backup_key(e))
			pl.Lock()
			has := syncer.HasBackup(l)
			var restore_err error
			if has {
				restore_err = syncer.CopyIn(ctx, d.cli, id, e.cfg_dir, l, e.item.Workspace, e.uid, e.gid)
			}
			pl.Unlock()
			if restore_err != nil {
				return fmt.Errorf("restore: %w", restore_err)
			}
			if has {
				d.log.Info("backup restored", slog.String("id", short(id)), slog.String("name", e.item.Name))
			}
		}
		e.restored = true
	}

	if err := d.install_gitconfig(ctx, e, id); err != nil {
		return fmt.Errorf("install gitconfig: %w", err)
	}

	// Lay down the host's shared Claude Code config (settings.json is the base
	// the seed below merges cld's keys onto). Best effort — a failure must not
	// block the session over an optional convenience.
	if err := d.install_claude_config(ctx, e, id); err != nil {
		d.log.Warn("share claude config failed",
			slog.String("name", e.item.Name), slog.String("error", err.Error()))
	}

	if err := d.seed_file(ctx, e, id, ".claude.json", 0o600, func(b []byte) ([]byte, error) {
		return claude.SeedState(b, e.item.Workspace)
	}); err != nil {
		return fmt.Errorf("seed state: %w", err)
	}
	if err := d.seed_file(ctx, e, id, "settings.json", 0o644, claude.SeedSettings); err != nil {
		return fmt.Errorf("seed settings: %w", err)
	}

	// Own the whole config tree to the container user. docker cp (which restore
	// and every WriteFile use) applies the tar's uid/gid only to entries it
	// names explicitly; any intermediate directory it has to create — projects/,
	// projects/<enc>/, file-history/ — it makes root-owned. claude runs as the
	// unprivileged user, so a root-owned projects/<enc>/ lets it resume (read)
	// but not create a new conversation's transcript (write), which shows up as
	// claude dying the moment you start a new conversation. Normalizing here
	// covers every current and future docker-cp path, not just the ones we know.
	out, code, err = dockerx.ExecOutput(ctx, d.cli, id, "0", []string{
		"chown", "-R", fmt.Sprintf("%d:%d", e.uid, e.gid), e.cfg_dir,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("chown config dir: exit %d: %s", code, out)
	}
	return nil
}

// seed_file reads a config-dir file, applies seed, and writes it back only if
// the content changed, owned by the container user.
func (d *Daemon) seed_file(ctx context.Context, e *entry, id string, name string, mode int64, seed func([]byte) ([]byte, error)) error {
	existing, _, err := dockerx.ReadFile(ctx, d.cli, id, path.Join(e.cfg_dir, name))
	if err != nil {
		return err
	}
	seeded, err := seed(existing)
	if err != nil {
		return err
	}
	if bytes.Equal(existing, seeded) {
		return nil
	}
	return dockerx.WriteFile(ctx, d.cli, id, e.cfg_dir, name, mode, e.uid, e.gid, seeded)
}

// read_config_file loads the devcontainer.json the container was built from,
// given the host path recorded in the devcontainer.config_file label. It first
// reads that host path directly — correct when the daemon runs on the host —
// and, when that fails (a containerized daemon cannot see host paths), falls
// back to reading the file from inside the running container, where the project
// is normally bind-mounted. The fall-back is best-effort: it needs the config
// to sit under one of the container's mounts, which most devcontainers satisfy
// but the spec does not guarantee. Returns nil when neither source is readable,
// leaving the workspace folder to be resolved from the mounts alone.
func (d *Daemon) read_config_file(ctx context.Context, id, host_path string, mounts []devc.Mount) []byte {
	if host_path == "" {
		return nil
	}
	if b, err := os.ReadFile(host_path); err == nil {
		return b
	}
	// The daemon could not read the host path (it is containerized): map the
	// config's host path into the container and read it there.
	in := devc.ContainerPath(host_path, mounts)
	if in == "" {
		return nil
	}
	b, ok, err := dockerx.ReadFile(ctx, d.cli, id, in)
	if err != nil || !ok {
		return nil
	}
	return b
}

// ensure_session creates the host tmux session whose pane runs cld's own
// exec-attach client, which runs claude inside the container.
func (d *Daemon) ensure_session(ctx context.Context, e *entry, id string) error {
	name := devc.SessionName(e.item.Name)
	has, err := d.tmux.HasSession(ctx, name)
	if err != nil || has {
		return err
	}

	enc := claude.EncodeProjectPath(e.item.Workspace)
	glob := tmuxx.Quote(path.Join(e.cfg_dir, "projects", enc)) + "/*.jsonl"
	_, code, err := dockerx.ExecOutput(ctx, d.cli, id, e.user, []string{
		"sh", "-c", "ls " + glob + " >/dev/null 2>&1",
	})
	if err != nil {
		return err
	}
	has_history := code == 0

	env := d.session_env(e)
	remote := with_unset(env.Unset(), session_command(has_history))

	argv := []string{
		d.self, "x", "exec",
		"--user", e.user,
		"--workdir", e.item.Workspace,
	}
	for _, kv := range env.Overrides() {
		argv = append(argv, "--env", kv)
	}
	argv = append(argv,
		"--notify", d.cfg.SocketPath(),
		"--session-gen", e.started_at,
		id, "--")
	argv = append(argv, remote...)

	command := tmuxx.QuoteAll(argv)
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		command = "DOCKER_HOST=" + tmuxx.Quote(h) + " " + command
	}

	if err := d.tmux.NewSession(ctx, name, command); err != nil {
		return err
	}
	// The session name is the stable identity (kept full for a namespaced
	// project), so point the window name — and thus the terminal tab — at the
	// terse display label instead, prefixed with the glyph that marks the tab as
	// a claude session (run_titler animates that glyph afterwards). Setting it
	// here shows the prefix from the first frame and, as a side effect, pins
	// off tmux's own renaming so the animator's renames are never clobbered.
	// Best-effort: a cosmetic tab name must not fail provisioning.
	tab := e.item.Display
	if tab == "" {
		tab = e.item.Name
	}
	if err := d.tmux.SetWindowName(ctx, name, windowName(titleGlyph, tab)); err != nil {
		d.log.Warn("set window name",
			slog.String("name", name), slog.String("error", err.Error()))
	}
	// Bind the session's split/new-window keys to a shell inside this same
	// container, so an extra pane lands in the container, not the host tmux
	// server. Same exec plumbing as the claude pane, minus the --notify/
	// --session-gen wiring (an ad-hoc shell exiting must not end the session)
	// and running a login shell instead of claude.
	if err := d.tmux.SetSplitCommand(ctx, name, d.split_command(e, id)); err != nil {
		return err
	}
	// Record that a live session now exists for this generation.
	d.sessions.set(id, sessionState{Gen: e.started_at, Ended: false})
	e.session_failed = false
	d.log.Info("session created", slog.String("name", name))
	return nil
}

// session_command is the argv run inside the container's tmux pane. With prior
// history it resumes the conversation, but ALWAYS with a fresh-session
// fallback: `claude --continue` exits immediately with "no conversation found
// to continue" whenever Claude Code has nothing it can resume — an empty or
// incompatible transcript, or a projects/ directory a newer Claude Code encodes
// differently than cld does. A bare `claude --continue` would then leave a dead
// pane that `cld it --new` only ever recreates into the same instant exit, so
// the `|| exec claude` keeps the session alive regardless. cld therefore never
// depends on correctly predicting Claude Code's resume behaviour to keep a
// session up.
func session_command(has_history bool) []string {
	if has_history {
		return []string{"sh", "-c", "claude --continue || exec claude"}
	}
	return []string{"claude"}
}

// split_command is the shell command line bound to a session's split and
// new-window keys: the same exec-attach client the claude pane runs, pointed
// at a login shell instead of claude. It carries the session env so an extra
// pane matches claude's environment, but omits --notify/--session-gen so the
// shell exiting is never mistaken for the user ending the session.
func (d *Daemon) split_command(e *entry, id string) string {
	argv := []string{
		d.self, "x", "exec",
		"--user", e.user,
		"--workdir", e.item.Workspace,
	}
	env := d.session_env(e)
	for _, kv := range env.Overrides() {
		argv = append(argv, "--env", kv)
	}
	argv = append(argv, id, "--")
	// Prefer the user's login shell, then bash, then sh — devcontainer images
	// vary. Resolve the shell BEFORE exec: a redirection on `exec` is inherited
	// by the replacing process, so silencing the exec's own failure there would
	// send the pane shell's stderr — and that of everything run in it — to
	// /dev/null for the pane's whole life. `|| exec sh` cannot serve as the
	// fallback either, since a failed `exec` terminates a non-interactive shell
	// before the `||` branch is reached. exec so the shell owns the pane and its
	// exit closes the pane.
	argv = append(argv, with_unset(env.Unset(),
		[]string{"sh", "-c", `s=${SHELL:-}; [ -x "$s" ] || s=$(command -v bash 2>/dev/null || command -v sh); exec "$s"`})...)

	command := tmuxx.QuoteAll(argv)
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		command = "DOCKER_HOST=" + tmuxx.Quote(h) + " " + command
	}
	return command
}

// Environment layer origins, as reported by `cld setting env`. They are worded
// as the place a user would go to change the value.
const (
	envOriginRemote  = "devcontainer remoteEnv"
	envOriginDefault = "cld default"
	envOriginDocker  = "cld docker"
	envOriginConfig  = "cld.yaml env"
	envOriginManaged = "cld"
)

// session_env resolves the environment every claude session runs with, from
// lowest precedence to highest: the container's own environment, the
// devcontainer's remoteEnv, cld's overridable defaults, the user's cld.yaml
// (globally, then per matching project), and finally the keys cld manages
// itself, which the config layer is not allowed to name.
//
// The daemon owns this policy in one place; the pane client just forwards it.
// Note this is a property of the processes cld starts — the claude pane, its
// split panes, and user scripts — not of the container: cld does not create
// the container, so it cannot change what everything else in it sees. See
// docs/session-env.md.
func (d *Daemon) session_env(e *entry) envx.Result {
	layers := []envx.Layer{
		{Origin: envOriginRemote, Vars: envPtrs(e.remote_env)},
		{Origin: envOriginDefault, Vars: envPtrs(d.env_defaults())},
	}
	// The engine cld runs for this project, if any. Below the config layers on
	// purpose: it is a default, not a promise, so a user who points DOCKER_HOST
	// at a real remote engine still wins — which is why DOCKER_HOST is not a
	// reserved key.
	if e.docker_host != "" {
		layers = append(layers, envx.Layer{
			Origin: envOriginDocker,
			Vars:   envPtrs(map[string]string{"DOCKER_HOST": e.docker_host}),
		})
	}
	layers = append(layers, d.env_user_layers(e)...)
	layers = append(layers, envx.Layer{Origin: envOriginManaged, Vars: envPtrs(d.env_managed(e))})
	return envx.Resolve(e.container_env, d.env_lookup, layers...)
}

// env_defaults are cld's opinions rather than its promises, so the config
// layer may override them: devcontainer images often lack a locale and
// claude's TUI needs UTF-8, and claude must not update itself out from under
// the version cld installed and reports.
func (d *Daemon) env_defaults() map[string]string {
	return map[string]string{
		"DISABLE_AUTOUPDATER": "1",
		"TERM":                "xterm-256color",
		"LANG":                "C.UTF-8",
	}
}

// env_user_layers are the layers declared in cld.yaml: the global env, then
// every project block whose globs match this workspace, in file order. They
// accumulate — a broad block and a narrow one compose rather than the first
// match winning.
func (d *Daemon) env_user_layers(e *entry) []envx.Layer {
	out := []envx.Layer{}
	if len(d.cfg.Env) > 0 {
		out = append(out, envx.Layer{Origin: envOriginConfig, Vars: d.cfg.Env})
	}
	for _, p := range d.cfg.MatchProjects(e.item.LocalFolder) {
		if len(p.Env) == 0 {
			continue
		}
		// Name the block by what it matched: an index would send the user
		// counting list items to find which one set a variable.
		out = append(out, envx.Layer{
			Origin: "cld.yaml projects[" + strings.Join(p.Match, ",") + "]",
			Vars:   p.Env,
		})
	}
	return out
}

// env_managed are the variables cld sets to keep its own promises: they point
// claude at the config cld seeded, the agent relay, the broker proxy, and the
// telemetry receiver. config.ReservedEnvKeys rejects them in user config, so
// this layer never actually contends with one — it is last regardless, because
// a session that quietly lost one of these would fail in ways no error message
// explains.
func (d *Daemon) env_managed(e *entry) map[string]string {
	env := map[string]string{
		"CLAUDE_CONFIG_DIR": e.cfg_dir,
	}
	// Point git at the host gitconfig copied into the config dir — but only
	// when one was installed, so we don't shadow the image's own ~/.gitconfig
	// with an empty file for users who have no host gitconfig.
	if e.git_config {
		env["GIT_CONFIG_GLOBAL"] = e.gitconfig_path()
	}
	// Point ssh clients (git signing/push) at the relay socket. Only when the
	// relay actually runs (arch match); otherwise leave whatever the container
	// already had.
	if d.cfg.Auth.ForwardAgentEnabled() && e.arch_ok {
		env["SSH_AUTH_SOCK"] = e.agent_sock()
	}
	// Route claude's API traffic through the broker's proxy: it authenticates
	// with a centrally-refreshed subscription token, so the session holds only a
	// placeholder and never a refresh token. ENABLE_TOOL_SEARCH re-enables the
	// tool-search optimization that a non-first-party base URL otherwise disables.
	if d.broker_session(e) {
		env["ANTHROPIC_BASE_URL"] = "http://" + proxyListenAddr
		env["ANTHROPIC_AUTH_TOKEN"] = "cld-broker-placeholder"
		env["ENABLE_TOOL_SEARCH"] = "true"
	}
	// Point claude's OpenTelemetry exporter at the receiver relay_otlp serves on
	// the container's loopback, so `cld ls` can show what the session consumed.
	// Only when the relay actually runs (arch match), else the exporter would
	// retry against a dead port for the life of the session.
	if d.telemetry_session(e) {
		env["CLAUDE_CODE_ENABLE_TELEMETRY"] = "1"
		// Metrics only: cld reads the two consumption counters and has no
		// use for logs or traces, which would otherwise carry prompt and
		// tool metadata across the relay for nothing.
		env["OTEL_METRICS_EXPORTER"] = "otlp"
		env["OTEL_LOGS_EXPORTER"] = "none"
		env["OTEL_TRACES_EXPORTER"] = "none"
		// JSON, not protobuf: it is what internal/otlpx decodes, which is
		// why the daemon needs no protobuf runtime to be the collector.
		env["OTEL_EXPORTER_OTLP_PROTOCOL"] = "http/json"
		env["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://" + otlpListenAddr
		env["OTEL_METRIC_EXPORT_INTERVAL"] = strconv.FormatInt(
			d.cfg.Telemetry.ExportIntervalOrDefault().Milliseconds(), 10)
		// The daemon attributes every export by the relay it arrived on, so
		// the identifying resource attributes are redundant. Dropping them
		// keeps the account uuid, account id and session id out of the
		// exports entirely rather than receiving and discarding them.
		env["OTEL_METRICS_INCLUDE_SESSION_ID"] = "false"
		env["OTEL_METRICS_INCLUDE_ACCOUNT_UUID"] = "false"
	}
	return env
}

// env_lookup answers the ${ns:NAME} references envx cannot resolve on its own.
//
// ${env:NAME} reads the DAEMON's own environment, which is how a secret
// reaches a session without being written into cld.yaml: put it in the
// daemon's compose file or `cld install` and reference it from there.
//
// ${localEnv:NAME}, which a devcontainer.json value may use, resolves to
// nothing: a containerized daemon cannot see the host user's environment, and
// silently substituting its own would be worse than an empty value.
func (d *Daemon) env_lookup(ns, name string) (string, bool) {
	switch ns {
	case "env":
		return os.LookupEnv(name)
	case "localEnv":
		d.log.Warn("env: ${localEnv:} cannot be resolved by the daemon",
			slog.String("name", name))
		return "", false
	default:
		return "", false
	}
}

// envPtrs adapts a plain map to an envx layer, where a nil value would mean
// "remove this variable" — something only user config can ask for.
func envPtrs(m map[string]string) map[string]*string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]*string, len(m))
	for k, v := range m {
		out[k] = &v
	}
	return out
}

// with_unset wraps a command so variables the config removed are gone from its
// environment. A docker exec can add and replace variables but never drop an
// inherited one, so the removal has to happen in the command itself. The keys
// need no quoting: config only accepts valid environment variable names.
func with_unset(keys []string, argv []string) []string {
	if len(keys) == 0 {
		return argv
	}
	// `exec "$@"` keeps the wrapped command in the same process, so its exit
	// status is still the pane's and nothing else has to know about the wrapper.
	return append([]string{
		"sh", "-c", "unset " + strings.Join(keys, " ") + `; exec "$@"`, "sh",
	}, argv...)
}

// telemetry_session reports whether this session should export its consumption
// metrics to the daemon. It needs the feature enabled and an arch match — the
// receiver rides the same in-container relay as the control API and the auth
// proxy, which only runs when cld's own binary can execute in the container.
func (d *Daemon) telemetry_session(e *entry) bool {
	return e.arch_ok && d.cfg.Telemetry.Enabled()
}

// recreate_session forces a fresh session for a container whose session the
// user had ended, backing `cld it --new`.
func (d *Daemon) recreate_session(ctx context.Context, e *entry) error {
	if e.cfg_dir == "" || e.item.Workspace == "" {
		return fmt.Errorf("container %q is not provisioned", e.item.Name)
	}
	d.sessions.clear(e.id)
	if e.item.Name != "" {
		d.tmux.KillSession(ctx, devc.SessionName(e.item.Name))
	}
	if err := d.ensure_session(ctx, e, e.id); err != nil {
		return err
	}
	e.session_done = true
	// A fresh live session exists again, so clear a prior ended/failed status
	// (ensure_session already cleared session_failed) and its error.
	if e.item.Status == StatusSessionEnded || e.item.Status == StatusFailed {
		e.item.Status = StatusReady
		e.item.Error = ""
	}
	// Reset the pushed activity to its resting state so the recreated session
	// does not inherit the ended one's last working/waiting: waiting if a resumed
	// transcript kept a title, else idle. claude's hooks take over from the first
	// prompt. Non-push entries are classified from the pane and need no reset.
	if e.activity_pushed {
		e.item.Activity = classifyActivity("", e.item.Title)
	}
	e.publish()
	return nil
}

// unique_name disambiguates display names that collide across containers,
// reading other entries through their published snapshot.
func (d *Daemon) unique_name(id string, name string) string {
	d.mu.Lock()
	others := make([]*entry, 0, len(d.entries))
	for other_id, other := range d.entries {
		if other_id != id {
			others = append(others, other)
		}
	}
	d.mu.Unlock()

	for _, other := range others {
		if other.snapshot().Name == name {
			return name + "-" + short(id)[:5]
		}
	}
	return name
}

// unique_alias returns stem when no other container already answers to it,
// otherwise stem with a growing prefix of Fingerprint(seed) appended — the
// git-short-hash approach — until it is free. Both other aliases AND other
// names are treated as taken, so a resolved alias never collides with any
// other container's handle and lookups stay unambiguous. The digest is derived
// from seed (the workspace path), so the same project recreated later lands on
// the same alias rather than a random one.
func (d *Daemon) unique_alias(id string, stem string, seed string) string {
	if stem == "" {
		stem = "dc"
	}

	d.mu.Lock()
	taken := make(map[string]struct{}, len(d.entries)*2)
	for other_id, other := range d.entries {
		if other_id == id {
			continue
		}
		s := other.snapshot()
		if s.Alias != "" {
			taken[s.Alias] = struct{}{}
		}
		if s.Name != "" {
			taken[s.Name] = struct{}{}
		}
	}
	d.mu.Unlock()

	if _, ok := taken[stem]; !ok {
		return stem
	}
	fp := devc.Fingerprint(seed)
	for n := 2; n <= len(fp); n++ {
		cand := stem + "-" + fp[:n]
		if _, ok := taken[cand]; !ok {
			return cand
		}
	}
	return stem + "-" + fp
}
