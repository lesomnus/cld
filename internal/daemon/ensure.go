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
		// Prefer the devcontainer.json "name"; fall back to the folder name.
		display := devc.Slug(e.dev_name)
		if display == "" {
			display = devc.Slug(devc.DisplayName(local_folder))
		}
		if display == "" {
			display = "devcontainer"
		}
		e.item.Name = d.unique_name(id, display)
		// A short handle for the container, derived from its name and kept
		// unique across the fleet by appending a digest of the workspace path.
		e.item.Alias = d.unique_alias(id, devc.Alias(display), local_folder)
		e.publish()
	}

	version, err := d.install_claude(ctx, e, id)
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

	if err := d.prepare_state(ctx, e, id); err != nil {
		return fmt.Errorf("prepare state: %w", err)
	}

	// Best-effort: give VS Code / Cursor a "claude" terminal profile that runs
	// `cld it`. Needs the in-container cld binary (arch match).
	if e.arch_ok {
		d.install_vscode_profile(ctx, e, id)
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
		} else {
			go d.poll_container(wctx, e)
		}
	}

	if e.item.Status == StatusProvisioning {
		e.item.Status = StatusReady
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
		// Prefer the devcontainer.json "name"; fall back to the folder name.
		display := devc.Slug(devc.ProjectName(config_file))
		if display == "" {
			display = devc.Slug(devc.DisplayName(local_folder))
		}
		if display == "" {
			display = "devcontainer"
		}
		e.item.Name = d.unique_name(e.id, display)
		e.item.Alias = d.unique_alias(e.id, devc.Alias(display), local_folder)
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
		d.copy_out(ctx, e, dirty{global: true, project: true})
	}
	if e.item.Name != "" {
		d.tmux.KillSession(ctx, devc.SessionName(e.item.Name))
	}
	e.session_done = false
	if e.item.Status != StatusSessionEnded {
		e.item.Status = StatusStopped
	}
	e.publish()
	d.log.Info("stopped", slog.String("id", short(e.id)), slog.String("name", e.item.Name))
}

// teardown handles a destroy (or a container that vanished from the listing):
// the container is gone for good, so finalize and drop the entry.
func (d *Daemon) teardown(ctx context.Context, e *entry) {
	d.stop(ctx, e)
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

	var config_file []byte
	if p := labels[devc.LabelConfigFile]; p != "" {
		config_file, _ = os.ReadFile(p)
	}
	e.dev_name = devc.ProjectName(config_file)
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
	e.arch_ok = arch == runtime.GOARCH
	return nil
}

// install_claude puts the current version into the container as
// claude-<version> and atomically points the "claude" symlink at it, so a
// running binary is never overwritten (ETXTBSY) and live sessions keep
// their version. The copy verifies the installed size to detect an
// interrupted earlier copy, and re-fetches the host binary if the cache was
// garbage-collected out from under it.
func (d *Daemon) install_claude(ctx context.Context, e *entry, id string) (string, error) {
	version, bin, err := d.rel.Ensure(ctx, e.platform)
	if err != nil {
		return "", err
	}

	name := "claude-" + version
	if err := d.install_binary(ctx, id, bin, name); err != nil {
		// The cache may have been GC'd between Ensure and open; retry once.
		if os.IsNotExist(err) {
			if _, bin, err = d.rel.Ensure(ctx, e.platform); err != nil {
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
			d.global_mu.RLock()
			has := syncer.HasBackup(l)
			var restore_err error
			if has {
				restore_err = syncer.CopyIn(ctx, d.cli, id, e.cfg_dir, l, e.item.Workspace, e.uid, e.gid)
			}
			d.global_mu.RUnlock()
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

	if err := d.bootstrap_credentials(ctx, e, id); err != nil {
		return fmt.Errorf("bootstrap credentials: %w", err)
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

// bootstrap_credentials copies credentials from the container's default
// ~/.claude (typically a user's legacy bind mount) into cld's config dir when
// the latter has none — so existing setups keep working with zero logins
// while cld's dir stays authoritative for everything else.
func (d *Daemon) bootstrap_credentials(ctx context.Context, e *entry, id string) error {
	dst := path.Join(e.cfg_dir, ".credentials.json")
	if _, ok, err := dockerx.ReadFile(ctx, d.cli, id, dst); err != nil || ok {
		return err
	}

	legacy := path.Join(claude.LegacyConfigDirIn(e.home), ".credentials.json")
	data, ok, err := dockerx.ReadFile(ctx, d.cli, id, legacy)
	if err != nil || !ok {
		return err
	}

	if err := dockerx.WriteFile(ctx, d.cli, id, e.cfg_dir, ".credentials.json", 0o600, e.uid, e.gid, data); err != nil {
		return err
	}
	d.log.Info("credentials bootstrapped from ~/.claude",
		slog.String("id", short(id)), slog.String("name", e.item.Name))
	return nil
}

// oauth_token_file resolves the host file whose OAuth token is injected into
// sessions as CLAUDE_CODE_OAUTH_TOKEN. A token set via `cld auth set-token`
// (stored under DataDir) takes precedence over the statically configured
// auth.oauth_token_file, so a container-side login wins over stale config.
// Returns "" when neither is present, leaving the session to its own credentials.
func (d *Daemon) oauth_token_file() string {
	if p := d.cfg.OAuthTokenStorePath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return d.cfg.Auth.OAuthTokenFile
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

	remote := session_command(has_history)

	argv := []string{
		d.self, "x", "exec",
		"--user", e.user,
		"--workdir", e.item.Workspace,
	}
	for _, kv := range d.session_env(e) {
		argv = append(argv, "--env", kv)
	}
	if f := d.oauth_token_file(); f != "" {
		argv = append(argv, "--oauth-token-file", f)
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

// session_env is the environment injected into every claude session. The
// daemon owns this policy in one place; the pane client just forwards it.
func (d *Daemon) session_env(e *entry) []string {
	env := []string{
		"CLAUDE_CONFIG_DIR=" + e.cfg_dir,
		"DISABLE_AUTOUPDATER=1",
		"TERM=xterm-256color",
		// Devcontainer images often lack a locale; claude's TUI needs UTF-8.
		"LANG=C.UTF-8",
	}
	// Point git at the host gitconfig copied into the config dir — but only
	// when one was installed, so we don't shadow the image's own ~/.gitconfig
	// with an empty file for users who have no host gitconfig.
	if e.git_config {
		env = append(env, "GIT_CONFIG_GLOBAL="+e.gitconfig_path())
	}
	// Point ssh clients (git signing/push) at the relay socket. Only when the
	// relay actually runs (arch match); otherwise leave whatever the container
	// already had.
	if d.cfg.Auth.ForwardAgentEnabled() && e.arch_ok {
		env = append(env, "SSH_AUTH_SOCK="+e.agent_sock())
	}
	return env
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
