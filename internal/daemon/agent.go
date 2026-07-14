package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/lesomnus/cld/internal/agentx"
	"github.com/lesomnus/cld/internal/claude"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// AgentSocketName is where the relay listens inside the container; the session
// points SSH_AUTH_SOCK at it.
const AgentSocketName = "agent.sock"

func (e *entry) agent_sock() string {
	return path.Join(e.cfg_dir, AgentSocketName)
}

// api_sock is where the daemon relays its own control API inside the container:
// the default path an in-container `cld` dials for the daemon
// (<cache>/cld/cld.sock, cache resolved like os.UserCacheDir), so `cld it`
// there needs no configuration.
func (e *entry) api_sock() string {
	return path.Join(e.cache_home, "cld", "cld.sock")
}

// agent_source returns the socket path of the ssh-agent to forward, resolved
// fresh each call so a changed agent (new login session) is picked up. It
// prefers the exported socket (kept current by `cld it`/`up` via `cld agent
// export`, and the only option for a compose daemon) over the daemon's own
// SSH_AUTH_SOCK, which goes stale once the login session that started it ends.
// Each candidate is probed for liveness: a unix socket leaves its file behind
// on an unclean exit, so a mere stat would happily return a dead exporter and
// shadow a working fallback.
func (d *Daemon) agent_source() string {
	for _, s := range []string{d.cfg.AgentSocketPath(), os.Getenv("SSH_AUTH_SOCK")} {
		if s != "" && socket_alive(s) {
			return s
		}
	}
	return ""
}

func socket_alive(p string) bool {
	c, err := net.DialTimeout("unix", p, 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func (d *Daemon) agent_dial(ctx context.Context) (net.Conn, error) {
	src := d.agent_source()
	if src == "" {
		return nil, errors.New("no ssh-agent to forward")
	}
	var dl net.Dialer
	return dl.DialContext(ctx, "unix", src)
}

// GitConfigName is where the staged ~/.gitconfig lands in the config dir;
// sessions point GIT_CONFIG_GLOBAL at it.
const GitConfigName = "gitconfig"

func (e *entry) gitconfig_path() string {
	return path.Join(e.cfg_dir, GitConfigName)
}

// install_gitconfig copies the host gitconfig staged by `cld it`/`up` into the
// session config dir, so container git shares the host identity and signing
// setup (VS Code Dev Containers parity). Absent staging is not an error.
func (d *Daemon) install_gitconfig(ctx context.Context, e *entry, id string) error {
	data, err := os.ReadFile(d.cfg.GitConfigPath())
	if err != nil {
		return nil
	}
	data = strip_credential_helpers(data)
	if err := dockerx.WriteFile(ctx, d.cli, id, e.cfg_dir, GitConfigName, 0o644, e.uid, e.gid, data); err != nil {
		return err
	}
	e.git_config = true
	return nil
}

// claudeShareDirs are the user-default customization directories mirrored
// into a session; settings.json and CLAUDE.md are handled separately.
var claudeShareDirs = []string{"commands", "agents", "output-styles"}

// install_claude_config mirrors cld's own user-default config (see
// config.Config.UserDefaultDir — a directory owned by cld, not the host's
// ~/.claude) into the session config dir: the user settings.json (sanitized —
// see SanitizeUserSettings — with cld's own keys merged afterwards by
// seed_file), the personal CLAUDE.md, and the commands/, agents/, and
// output-styles/ directories. CLAUDE_CONFIG_DIR is the config dir, so claude
// reads all of these as user-level config. It is a mirror: an item removed
// from user-default is removed in the container too (except settings.json,
// which cld always seeds). It is best-effort — the caller only logs a failure
// — so it collects per-item errors and keeps going rather than leaving a
// half-applied config. Ownership is normalized by the final chown.
func (d *Daemon) install_claude_config(ctx context.Context, e *entry, id string) error {
	if !d.cfg.Auth.ShareConfigEnabled() {
		return nil
	}
	share := d.cfg.UserDefaultDir()
	var errs []error

	// settings.json: the user-default file as the base seed_file merges cld's
	// keys onto, after dropping secret/host-only keys (in case it was copied
	// verbatim from a real ~/.claude/settings.json). An unparseable file is
	// skipped (never written) so it cannot fail the settings seed and block
	// the session.
	if data, err := os.ReadFile(filepath.Join(share, "settings.json")); err == nil {
		if clean, ok := claude.SanitizeUserSettings(data); ok {
			if err := dockerx.WriteFile(ctx, d.cli, id, e.cfg_dir, "settings.json", 0o644, e.uid, e.gid, clean); err != nil {
				errs = append(errs, err)
			}
		}
	}

	// CLAUDE.md and the dirs are mirrored: present in user-default → install;
	// absent → remove the container copy (which a restore may have brought
	// back).
	if data, err := os.ReadFile(filepath.Join(share, "CLAUDE.md")); err == nil {
		if err := dockerx.WriteFile(ctx, d.cli, id, e.cfg_dir, "CLAUDE.md", 0o644, e.uid, e.gid, data); err != nil {
			errs = append(errs, err)
		}
	} else if err := d.remove_in_container(ctx, id, path.Join(e.cfg_dir, "CLAUDE.md")); err != nil {
		errs = append(errs, err)
	}

	for _, name := range claudeShareDirs {
		if err := d.remove_in_container(ctx, id, path.Join(e.cfg_dir, name)); err != nil {
			errs = append(errs, err)
			continue
		}
		src := filepath.Join(share, name)
		if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
			continue // user-default has no such dir; the container's is now cleared
		}
		if err := dockerx.CopyDirToContainer(ctx, d.cli, id, e.cfg_dir, name, src, e.uid, e.gid); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// remove_in_container removes p inside the container (as root), tolerating a
// missing path.
func (d *Daemon) remove_in_container(ctx context.Context, id, p string) error {
	out, code, err := dockerx.ExecOutput(ctx, d.cli, id, "0", []string{"rm", "-rf", p})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("rm %s: exit %d: %s", p, code, out)
	}
	return nil
}

// strip_credential_helpers removes every `helper` entry under a [credential]
// section from a gitconfig. The host's credential helper (gopass, osxkeychain,
// manager, …) is a host-only binary that does not exist in the container, so
// forwarding it would break HTTPS git auth with a confusing "not a git command"
// error. In-container git uses the forwarded ssh-agent (an SSH remote) instead.
func strip_credential_helpers(cfg []byte) []byte {
	lines := strings.Split(string(cfg), "\n")
	out := lines[:0]
	in_credential := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			// Section name is up to the first space, '"', or ']'; it is
			// case-insensitive in git.
			name := trimmed[1:]
			if i := strings.IndexAny(name, " \t\"]"); i >= 0 {
				name = name[:i]
			}
			in_credential = strings.EqualFold(name, "credential")
			out = append(out, line)
			continue
		}
		if in_credential {
			key := trimmed
			if i := strings.IndexByte(key, '='); i >= 0 {
				key = strings.TrimSpace(key[:i])
			}
			if strings.EqualFold(key, "helper") {
				continue // drop the host-only helper
			}
		}
		out = append(out, line)
	}
	return []byte(strings.Join(out, "\n"))
}

// relay_agent forwards the host ssh-agent into the container (SSH_AUTH_SOCK).
// relay_api exposes the daemon's own control API there, so `cld` run inside the
// container (e.g. `cld it`) can reach the daemon. Both use one mechanism —
// agentx over a `docker exec` stream — differing only in the container-side
// socket and the daemon-side dial target.
func (d *Daemon) relay_agent(ctx context.Context, e *entry, id string) {
	d.relay(ctx, e, id, "agent", []string{path.Join(install_dir, "cld"), "x", "agent", e.agent_sock()}, d.agent_dial)
}

func (d *Daemon) relay_api(ctx context.Context, e *entry, id string) {
	if !d.cfg.Auth.RemoteControlEnabled() {
		return
	}
	// Bridge the container's connections to a per-container SCOPED API served
	// in-process, so the container reaches only its own session — never the full
	// control socket (which could see or act on every other project). Identity
	// is bound to id here, not supplied by the container.
	ln := new_pipe_listener()
	srv := &http.Server{Handler: d.scoped_api(id)}
	go srv.Serve(ln)
	defer srv.Close()
	defer ln.Close()

	d.relay(ctx, e, id, "api",
		[]string{path.Join(install_dir, "cld"), "x", "api", e.api_sock()}, ln.dial)
}

// proxyListenAddr is the loopback address the auth proxy listens on INSIDE each
// container; claude reaches it via ANTHROPIC_BASE_URL. Each container has its own
// network namespace, so a fixed port never collides across containers.
const proxyListenAddr = "127.0.0.1:49327"

// broker_session reports whether this session should authenticate through the
// broker's proxy rather than logging in per container. The proxy is opt-in per
// project (see proxyStore): a session uses it only when the project explicitly
// enabled it via `cld up --proxy` / `cld it --proxy`, AND a broker login exists
// (`cld auth login`), AND the in-container relay can run (arch match, so cld's
// own binary can serve the proxy listener there). Absent any of these the
// session falls back to the default per-container login.
func (d *Daemon) broker_session(e *entry) bool {
	return e.arch_ok && d.cfg.Auth.RemoteControlEnabled() &&
		d.proxy.get(d.backup_key(e)) && d.broker.HasCredentials()
}

// relay_proxy exposes the daemon's auth proxy inside the container: an
// in-process reverse proxy that rewrites Authorization with the current
// subscription access token and forwards to api.anthropic.com. The container
// side listens on a loopback TCP port (claude points ANTHROPIC_BASE_URL at it);
// the daemon serves the proxy over a pipe listener, exactly like relay_api but
// with the broker handler instead of the scoped control API. The refresh token
// stays on the daemon: only short-lived access tokens ever cross the wire.
func (d *Daemon) relay_proxy(ctx context.Context, e *entry, id string) {
	if !d.broker_session(e) {
		return
	}
	ln := new_pipe_listener()
	srv := &http.Server{Handler: d.broker.Handler()}
	go srv.Serve(ln)
	defer srv.Close()
	defer ln.Close()

	d.relay(ctx, e, id, "proxy",
		[]string{path.Join(install_dir, "cld"), "x", "proxy", proxyListenAddr}, ln.dial)
}

// relay keeps a socket relay alive for the life of the container: each attempt
// runs the given container-side listener command and bridges its accepted
// connections to dial() on the daemon side, retrying while the container runs.
func (d *Daemon) relay(ctx context.Context, e *entry, id string, kind string, cmd []string, dial func(context.Context) (net.Conn, error)) {
	for ctx.Err() == nil {
		err := d.relay_once(ctx, id, e.user, cmd, dial)
		if ctx.Err() != nil {
			return
		}

		insp, ierr := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if ierr != nil {
			if cerrdefs.IsNotFound(ierr) {
				return // container really is gone; a clean-EOF end is expected
			}
			// A transient inspect failure must not permanently kill the relay
			// (in-container `cld it` depends on it): fall through and retry.
		} else if insp.Container.State == nil || !insp.Container.State.Running {
			return // container stopped
		}

		// The container is still up, so this really was a relay failure.
		if err != nil && !errors.Is(err, io.EOF) {
			d.log.Warn(kind+" relay error",
				slog.String("name", e.item.Name), slog.String("error", err.Error()))
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (d *Daemon) relay_once(ctx context.Context, id string, user string, cmd []string, dial func(context.Context) (net.Conn, error)) error {
	created, err := d.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		User:         user,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	})
	if err != nil {
		return err
	}

	att, err := d.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return err
	}
	defer att.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			att.Close()
		case <-done:
		}
	}()

	// att.Conn writes the exec's stdin (frames to the container listener);
	// att.Reader is the multiplexed stdout that stdcopy demuxes back to frames.
	pr, pw := io.Pipe()
	var errbuf lineBuffer
	go func() {
		_, e := stdcopy.StdCopy(pw, &errbuf, att.Reader)
		pw.CloseWithError(e)
	}()

	err = agentx.Bridge(ctx, att.Conn, pr, dial)
	if s := errbuf.String(); s != "" {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(s))
	}
	return err
}
