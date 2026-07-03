package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/devcup"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
	"github.com/moby/moby/client"
)

func NewCmdUp() *xli.Command {
	return &xli.Command{
		Name:  "up",
		Brief: "create/start a devcontainer and attach to its claude session",
		Flags: flg.Flags{
			&flg.Switch{Name: "no-attach", Brief: "provision only; do not attach"},
		},
		Args: arg.Args{
			&arg.String{Name: "path", Brief: "project folder (default: current directory)", Optional: true},
			&arg.Remains{Name: "args", Brief: "extra arguments for `devcontainer up`", Optional: true},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			path := "."
			arg.VisitP(cmd, "path", &path)
			workspace, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			if !devcup.HasConfig(workspace) {
				return fmt.Errorf("no devcontainer configuration in %s"+
					" (expected .devcontainer/devcontainer.json or .devcontainer.json)", workspace)
			}

			extra, _ := arg.Get[[]string](cmd, "args")
			o := devcup.Options{
				Workspace:   workspace,
				Args:        extra,
				RunnerImage: c.Up.Image,
				Stdout:      cmd,
				Stderr:      cmd.ErrWriter,
			}

			cli, err := client.New(client.FromEnv)
			if err != nil {
				return z.Err(err, "docker client")
			}
			defer cli.Close()

			runner := devcup.Resolve(o, exec.LookPath, func(ctx context.Context) error {
				return devcup.RunContainerized(ctx, cli, o)
			})
			fmt.Fprintln(cmd.ErrWriter, "cld: using", runner.Desc)
			if err := runner.Run(ctx); err != nil {
				return z.Err(err, "devcontainer up")
			}

			// The daemon picks the container up from the start event; wait for
			// it so the user lands in a working session.
			item, err := wait_ready(ctx, cmd, c.SocketPath(), workspace)
			if err != nil {
				return err
			}

			if v, _ := flg.Get[bool](cmd, "no-attach"); v || item == nil {
				return nil
			}

			// `up` means "bring it up and drop me in", so if the user had
			// ended the session earlier the daemon won't have recreated it —
			// recreate now so there is a session to attach to.
			if item.Status == daemon.StatusSessionEnded {
				if err := daemon.RecreateSession(ctx, c.SocketPath(), item.Name); err != nil {
					return err
				}
			}

			session := devc.SessionName(item.Name)
			ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
			info, ierr := daemon.FetchInfo(ictx, c.SocketPath())
			cancel()
			if ierr == nil && info.ContainerID != "" {
				return attach_via_exec(ctx, info, session, item.Name, c.SocketPath())
			}
			return attach_local(ctx, c.TmuxSocketPath(), session, item.Name, c.SocketPath())
		}),
	}
}

// wait_ready polls the daemon until the workspace's container is provisioned.
// A missing daemon is not an error: the devcontainer is up, which is already
// useful on its own. It returns (nil, nil) in that case.
//
// find swallows transient fetch errors (daemon starting up, momentary HTTP
// error) so the poll keeps trying; only a genuinely-absent daemon (no socket
// file) short-circuits, and only a Failed provisioning is fatal.
func wait_ready(ctx context.Context, cmd *xli.Command, socket string, workspace string) (*daemon.Item, error) {
	// No socket file means no daemon; a present socket that errors is treated
	// as transient below.
	if _, err := os.Stat(socket); err != nil {
		fmt.Fprintln(cmd.ErrWriter, "cld: devcontainer is up, but no daemon is reachable;"+
			" start `cld serve` to get a claude session")
		return nil, nil
	}

	find := func() (*daemon.Item, bool, error) {
		items, err := daemon.FetchItems(ctx, socket)
		if err != nil {
			return nil, false, nil // transient; retry
		}
		for _, it := range items {
			if it.LocalFolder != workspace {
				continue
			}
			switch it.Status {
			case daemon.StatusReady, daemon.StatusSessionEnded:
				return &it, true, nil
			case daemon.StatusFailed:
				return nil, true, fmt.Errorf("provisioning failed: %s", it.Error)
			}
			return &it, false, nil
		}
		return nil, false, nil
	}

	fmt.Fprintln(cmd.ErrWriter, "cld: waiting for provisioning (first run downloads claude)...")
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		item, done, err := find()
		if err != nil {
			return nil, err
		}
		if done {
			return item, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("timed out waiting for the daemon to provision %s; see `cld ls`", workspace)
}
