package daemon

import (
	"bytes"
	"context"
	"fmt"
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
// allow_session gates session creation to start events and first sight, so
// a session the user closed is not resurrected. It runs on the container's
// worker goroutine, so entry state needs no lock.
func (d *Daemon) ensure(ctx context.Context, e *entry, allow_session bool) {
	err := d.ensure_(ctx, e, allow_session)
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

func (d *Daemon) ensure_(ctx context.Context, e *entry, allow_session bool) error {
	id := e.id
	insp, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	c := insp.Container
	if c.State == nil || !c.State.Running {
		return nil
	}

	labels := map[string]string{}
	if c.Config != nil {
		labels = c.Config.Labels
	}
	local_folder := labels[devc.LabelLocalFolder]
	if local_folder == "" || devc.Ignored(labels, local_folder, d.cfg.Ignore) {
		d.remove(e)
		return nil
	}

	if e.item.Name == "" {
		e.item.LocalFolder = local_folder
		e.item.Name = d.unique_name(id, devc.DisplayName(local_folder))
	}

	// A container that already reached ready and has its session should not
	// pay the full provisioning cost on every reconcile; a user-ended
	// session must keep its status too.
	settled := e.item.Status == StatusReady || e.item.Status == StatusSessionEnded
	if settled && e.cfg_dir != "" && e.session_done && (e.bind_mounted || e.watch_stop != nil) {
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

	if allow_session && !e.session_done {
		if err := d.ensure_session(ctx, e, id); err != nil {
			return fmt.Errorf("session: %w", err)
		}
		e.session_done = true
	}

	if !e.bind_mounted && e.watch_stop == nil {
		wctx, stop := context.WithCancel(d.base_ctx)
		e.watch_stop = stop
		go d.sync_loop(wctx, e)
		if e.arch_ok {
			go d.watch_container(wctx, e, id)
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

// stop handles a die event: the container stopped but may start again. Tear
// down the session and watcher and take a final backup, but keep the entry so
// a restart is recognized.
func (d *Daemon) stop(ctx context.Context, e *entry) {
	if e.watch_stop != nil {
		e.watch_stop()
		e.watch_stop = nil
	}
	if !e.bind_mounted && e.item.Workspace != "" {
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
	d.remove(e)
	d.log.Info("retired", slog.String("id", short(e.id)), slog.String("name", e.item.Name))
}

// resolve figures out the effective user, its home, the config dir, the
// workspace path, the platform, and whether the config dir is bind-mounted.
func (d *Daemon) resolve(ctx context.Context, e *entry, id string, labels map[string]string, c *container_inspect) error {
	user := devc.RemoteUser(labels[devc.LabelMetadata])
	if user == "" && c.Config != nil {
		user = c.Config.User
	}
	if user == "" {
		user = "1000"
	}

	out, code, err := dockerx.ExecOutput(ctx, d.cli, id, user, []string{
		"sh", "-c", `echo "$(id -u):$(id -g):$HOME"`,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("probe user %q: exit %d: %s", user, code, out)
	}
	parts := strings.SplitN(strings.TrimSpace(out), ":", 3)
	if len(parts) != 3 {
		return fmt.Errorf("probe user %q: unexpected output %q", user, out)
	}
	e.user = user
	e.uid, _ = strconv.Atoi(parts[0])
	e.gid, _ = strconv.Atoi(parts[1])
	e.home = parts[2]
	if e.home == "" || e.home == "/" {
		return fmt.Errorf("user %q has no usable home", user)
	}
	e.cfg_dir = path.Join(e.home, claude.ConfigDirName)

	mounts := make([]devc.Mount, 0, len(c.Mounts))
	for _, m := range c.Mounts {
		mounts = append(mounts, devc.Mount{Source: m.Source, Destination: m.Destination})
		if m.Destination == e.cfg_dir {
			e.bind_mounted = true
		}
	}

	var config_file []byte
	if p := labels[devc.LabelConfigFile]; p != "" {
		config_file, _ = os.ReadFile(p)
	}
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

	out, code, err = dockerx.ExecOutput(ctx, d.cli, id, "", []string{
		"sh", "-c", `if [ -e /lib/ld-musl-x86_64.so.1 ] || [ -e /lib/ld-musl-aarch64.so.1 ]; then echo musl; else echo gnu; fi`,
	})
	if err != nil {
		return err
	}
	musl := code == 0 && strings.TrimSpace(out) == "musl"

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
	return version, nil
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
// leaves a truncated binary at the final path.
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
	return nil
}

// prepare_state restores the backup into a fresh container and seeds the
// onboarding, trust, and retention keys. Restore happens once per container
// generation and only when the container has no state of its own yet —
// a restarted container's state is newer than any backup.
func (d *Daemon) prepare_state(ctx context.Context, e *entry, id string) error {
	out, code, err := dockerx.ExecOutput(ctx, d.cli, id, "0", []string{
		"sh", "-c", fmt.Sprintf(`mkdir -p %s && chown %d:%d %s`,
			tmuxx.Quote(e.cfg_dir), e.uid, e.gid, tmuxx.Quote(e.cfg_dir)),
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("create config dir: exit %d: %s", code, out)
	}

	state_path := path.Join(e.cfg_dir, ".claude.json")
	if !e.bind_mounted && !e.restored {
		_, ok, err := dockerx.ReadFile(ctx, d.cli, id, state_path)
		if err != nil {
			return err
		}
		if !ok {
			l := d.layout(e)
			d.global_mu.RLock()
			has := syncer.HasBackup(l)
			var restore_err error
			if has {
				restore_err = syncer.CopyIn(ctx, d.cli, id, e.cfg_dir, l, e.item.Workspace, e.uid, e.gid)
			}
			d.global_mu.RUnlock()
			if restore_err != nil {
				return fmt.Errorf("restore: %w", restore_err)
			}
			if has {
				d.log.Info("backup restored", slog.String("id", short(id)), slog.String("name", e.item.Name))
			}
		}
		e.restored = true
	}

	if err := d.seed_file(ctx, e, id, ".claude.json", 0o600, func(b []byte) ([]byte, error) {
		return claude.SeedState(b, e.item.Workspace)
	}); err != nil {
		return fmt.Errorf("seed state: %w", err)
	}
	if err := d.seed_file(ctx, e, id, "settings.json", 0o644, claude.SeedSettings); err != nil {
		return fmt.Errorf("seed settings: %w", err)
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

	remote := []string{"claude"}
	if has_history {
		remote = append(remote, "--continue")
	}

	argv := []string{
		d.self, "x", "exec",
		"--user", e.user,
		"--workdir", e.item.Workspace,
		"--config-dir", e.cfg_dir,
		"--notify", d.cfg.SocketPath(),
		id, "--",
	}
	argv = append(argv, remote...)

	command := tmuxx.QuoteAll(argv)
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		command = "DOCKER_HOST=" + tmuxx.Quote(h) + " " + command
	}

	if err := d.tmux.NewSession(ctx, name, command); err != nil {
		return err
	}
	d.log.Info("session created", slog.String("name", name))
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
