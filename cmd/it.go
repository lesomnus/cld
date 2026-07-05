package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/termx"
	"github.com/lesomnus/cld/internal/tmuxx"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
	"github.com/moby/moby/client"
)

func NewCmdIt() *xli.Command {
	return &xli.Command{
		Name:  "it",
		Brief: "attach to the claude session of a devcontainer",
		Flags: flg.Flags{
			&flg.Switch{Name: "new", Brief: "recreate the session if the user had ended it"},
		},
		Args: arg.Args{
			&arg.String{Name: "name", Brief: "devcontainer name (`cld ls`); default: the only one / this container's own", Optional: true},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			name, _ := arg.Get[string](cmd, "name")
			if name == "" {
				// No name: attach to the sole devcontainer. Inside a managed
				// container the relayed listing is scoped to that one container,
				// so a bare `cld it` there means "attach to my own session" —
				// which is what a VS Code terminal profile runs.
				resolved, err := sole_devcontainer(ctx, c.SocketPath())
				if err != nil {
					return err
				}
				name = resolved
			} else if t := find_target(ctx, c.SocketPath(), name); t != nil {
				// Accept a short alias too: resolve it to the canonical name so
				// the session name and daemon lookups all key off the same one.
				name = t.Name
			}
			session := devc.SessionName(name)

			// Name this window after the devcontainer so multiple `cld it`
			// windows are distinguishable. The daemon's tmux runs with
			// set-titles off, so it leaves this outer title alone.
			termx.SetTitle(name)

			if v, _ := flg.Get[bool](cmd, "new"); v {
				if err := daemon.RecreateSession(ctx, c.SocketPath(), name); err != nil {
					return err
				}
			}

			// Publish this login session's ssh-agent for forwarding into the
			// container before we hand the terminal over.
			prepareHostShare(c)

			// Ask the daemon where the tmux server lives and how to attach.
			// When the daemon runs in a container this host can see, attach
			// through a docker exec into it — the host needs no tmux at all.
			// When we cannot see that container (e.g. we ARE a managed
			// container reaching the daemon through the in-container relay),
			// let the daemon stream the attach over the control socket. Without
			// a reachable daemon, fall back to a local tmux attach.
			ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
			info, ierr := daemon.FetchInfo(ictx, c.SocketPath())
			cancel()
			if ierr == nil && info.ContainerID != "" && daemon_container_reachable(ctx, info.ContainerID) {
				return attach_via_exec(ctx, info, session, name, c.SocketPath())
			}
			if ierr == nil && info.APIAttach {
				return daemon.AttachSession(ctx, c.SocketPath(), name)
			}
			return attach_local(ctx, c.TmuxSocketPath(), session, name, c.SocketPath())
		}),
	}
}

// sole_devcontainer returns the name of the one devcontainer the daemon serves,
// for a bare `cld it`. Inside a managed container the relayed listing is scoped
// to that container, so this resolves to the caller's own session; on a host it
// works when there is exactly one.
func sole_devcontainer(ctx context.Context, socket string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	items, err := daemon.FetchItems(ctx, socket)
	if err != nil {
		return "", err
	}
	switch len(items) {
	case 0:
		return "", fmt.Errorf("no devcontainer found; run `cld up` or see `cld ls`")
	case 1:
		return items[0].Name, nil
	default:
		names := make([]string, len(items))
		for i, it := range items {
			names[i] = it.Name
		}
		return "", fmt.Errorf("multiple devcontainers; name one: %s", strings.Join(names, ", "))
	}
}

// find_target maps a user-supplied handle — a display name or a short alias —
// to the devcontainer it refers to, matching NAME before ALIAS so a name always
// wins. It returns nil when nothing matches or the daemon is unreachable, which
// lets callers fall back to their existing behavior (treat the handle verbatim
// and surface the daemon's own not-found error).
func find_target(ctx context.Context, socket string, handle string) *daemon.Item {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	items, err := daemon.FetchItems(ctx, socket)
	if err != nil {
		return nil
	}
	for i := range items {
		if items[i].Name == handle {
			return &items[i]
		}
	}
	for i := range items {
		if items[i].Alias == handle {
			return &items[i]
		}
	}
	return nil
}

// daemon_container_reachable reports whether this host's docker can see the
// daemon's own container, i.e. whether a docker-exec attach into it is viable.
// It is false when `cld it` runs inside a managed container reaching the daemon
// only through the relay, which is what routes such calls to the API attach.
func daemon_container_reachable(ctx context.Context, id string) bool {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return false
	}
	defer cli.Close()
	ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err = cli.ContainerInspect(ictx, id, client.ContainerInspectOptions{})
	return err == nil
}

// attach_local runs the local tmux client against the shared server socket:
// the daemon runs on this host.
func attach_local(ctx context.Context, tmux_socket string, session string, name string, api_socket string) error {
	tmux := &tmuxx.Server{Socket: tmux_socket}
	has, err := tmux.HasSession(ctx, session)
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf("no session for %q: %s", name, hint(ctx, api_socket, name))
	}

	bin, err := exec.LookPath("tmux")
	if err != nil {
		return err
	}

	// Drop TMUX so attaching from inside another tmux works.
	env := []string{}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		env = append(env, kv)
	}
	return syscall.Exec(bin, tmux.AttachArgv(session), env)
}

// attach_via_exec attaches to the tmux server inside the daemon's container
// with cld's own exec-attach client, as the daemon's uid (tmux rejects other
// users' clients).
func attach_via_exec(ctx context.Context, info *daemon.Info, session string, name string, api_socket string) error {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return z.Err(err, "docker client")
	}
	defer cli.Close()

	term := os.Getenv("TERM")
	if term == "" {
		term = "xterm-256color"
	}

	code, err := termx.Run(ctx, cli, termx.ExecOptions{
		Container: info.ContainerID,
		User:      strconv.Itoa(info.UID),
		// Without a UTF-8 locale tmux renders every non-ASCII glyph to this
		// client as "_".
		Env: []string{"TERM=" + term, "LC_ALL=C.UTF-8"},
		Cmd: []string{"tmux", "-S", info.TmuxSocket, "attach-session", "-t", "=" + session},
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("no session for %q: %s", name, hint(ctx, api_socket, name))
	}
	os.Exit(0)
	return nil
}

// hint explains why a session is missing, best-effort via the daemon.
func hint(ctx context.Context, socket string, name string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	items, err := daemon.FetchItems(ctx, socket)
	if err != nil {
		return "is `cld serve` running?"
	}
	for _, it := range items {
		if it.Name != name {
			continue
		}
		switch it.Status {
		case daemon.StatusSessionEnded:
			return "the session was closed; reattach with `cld it --new " + name + "`"
		case daemon.StatusProvisioning:
			return "still provisioning; try again in a moment"
		case daemon.StatusFailed:
			return "provisioning failed: " + it.Error
		default:
			return fmt.Sprintf("status is %q", it.Status)
		}
	}
	return "no such devcontainer; see `cld ls`"
}
