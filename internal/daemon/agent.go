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
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/lesomnus/cld/internal/agentx"
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
	if err := dockerx.WriteFile(ctx, d.cli, id, e.cfg_dir, GitConfigName, 0o644, e.uid, e.gid, data); err != nil {
		return err
	}
	e.git_config = true
	return nil
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
