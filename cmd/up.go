package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/devcup"
	"github.com/lesomnus/cld/internal/tui"
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
			extra, _ := arg.Get[[]string](cmd, "args")

			// Unless the user already pointed the CLI at a config, discover the
			// workspace's devcontainer config(s) and, when there is more than
			// one, let them pick which to provision from.
			if !hasConfigArg(extra) {
				picked, proceed, err := resolveConfig(cmd, workspace)
				if err != nil {
					return err
				}
				if !proceed {
					return nil // the user cancelled the picker
				}
				extra = append(extra, picked...)
			}

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

			// Stage gitconfig and the ssh-agent BEFORE creating the container,
			// so the daemon has them when it provisions the new container (its
			// install_gitconfig and agent relay both read the cache dir).
			prepareHostShare(c)

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

			return attachTo(ctx, c, item.Name)
		}),
	}
}

// hasConfigArg reports whether the extra args already point the devcontainer
// CLI at a config, in which case cld leaves discovery/selection alone.
func hasConfigArg(args []string) bool {
	for _, a := range args {
		if a == "--config" || a == "--override-config" ||
			strings.HasPrefix(a, "--config=") || strings.HasPrefix(a, "--override-config=") {
			return true
		}
	}
	return false
}

// resolveConfig decides which devcontainer config `up` should provision from
// and returns the extra `devcontainer up` args needed to select it. proceed is
// false when the user cancels the picker (the caller should stop without
// provisioning). The cases:
//
//   - none:     write the built-in default into the workspace, no extra args.
//   - one:      a standard-location config needs nothing (the CLI auto-detects
//     it); a lone sub-folder config is passed with --config.
//   - multiple: prompt with a TUI picker on a terminal, and pass the pick with
//     --config so it is honored over the CLI's own auto-detection; without a
//     terminal, error and ask the user to pass --config explicitly.
func resolveConfig(cmd *xli.Command, workspace string) (extra []string, proceed bool, err error) {
	configs := devcup.DiscoverConfigs(workspace)
	switch len(configs) {
	case 0:
		// A workspace without its own config is provisioned from a built-in
		// minimal default named after the directory, written into the workspace
		// at .devcontainer/devcontainer.json (best-effort git-excluded). Writing
		// a real file — rather than an ephemeral --override-config — means the
		// devcontainer.config_file label points at a path the CLI, VS Code, and
		// the daemon can all read back, so the container is openable in VS Code.
		p, err := devcup.WriteDefaultConfig(workspace)
		if err != nil {
			return nil, false, z.Err(err, "prepare default devcontainer config")
		}
		fmt.Fprintf(cmd.ErrWriter,
			"%s: no devcontainer config in %s; wrote built-in default to %s (name=%s)\n",
			tui.Tag(), workspace, p, filepath.Base(workspace))
		return nil, true, nil

	case 1:
		c := configs[0]
		if c.Standard {
			return nil, true, nil // the CLI auto-detects it; nothing to add
		}
		// A lone sub-folder config is not auto-detected — point the CLI at it.
		fmt.Fprintf(cmd.ErrWriter, "%s: using devcontainer config %s\n", tui.Tag(), c.Rel)
		return []string{"--config", c.Path}, true, nil

	default:
		if !tui.Interactive() {
			return nil, false, fmt.Errorf(
				"found %d devcontainer configs in %s; pass one explicitly, e.g. `--config %s`",
				len(configs), workspace, configs[0].Rel)
		}
		choices := make([]tui.Choice, len(configs))
		for i, c := range configs {
			desc := "sub-folder config"
			if c.Standard {
				desc = "standard location"
			}
			choices[i] = tui.Choice{Title: c.Rel, Desc: desc}
		}
		idx, err := tui.Select("Select a devcontainer config", choices)
		if errors.Is(err, tui.ErrAborted) {
			fmt.Fprintf(cmd.ErrWriter, "%s: aborted\n", tui.Tag())
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		c := configs[idx]
		fmt.Fprintf(cmd.ErrWriter, "%s: using devcontainer config %s\n", tui.Tag(), c.Rel)
		// Pass --config even for a standard location so the pick wins over the
		// CLI preferring .devcontainer/devcontainer.json when both exist.
		return []string{"--config", c.Path}, true, nil
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
