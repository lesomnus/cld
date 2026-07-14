package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/installer"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
	"github.com/moby/moby/client"
)

func NewCmdInstall() *xli.Command {
	return &xli.Command{
		Name:  "install",
		Brief: "run the cld daemon as a container on this host's Docker",
		Flags: flg.Flags{
			&flg.Switch{Name: "recreate", Brief: "replace an existing daemon container"},
			&flg.String{Name: "image", Brief: "daemon image (default: config's install.image)"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			image := c.Install.Image
			flg.VisitP(cmd, "image", &image)
			recreate, _ := flg.Get[bool](cmd, "recreate")

			// Pre-create the shared dirs as this user so the daemon's sockets
			// and backups land under paths the host `cld` owns and reaches.
			os.MkdirAll(c.CacheDir, 0o755)
			os.MkdirAll(c.DataDir, 0o755)

			spec, err := installer.SpecFor(image, os.Getenv("DOCKER_HOST"), c.CacheDir, c.DataDir, os.Getuid(), os.Getgid())
			if err != nil {
				return err
			}

			cli, err := client.New(client.FromEnv)
			if err != nil {
				return z.Err(err, "docker client")
			}
			defer cli.Close()

			id, err := installer.Install(ctx, cli, spec, recreate, cmd.ErrWriter)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrWriter, "cld: daemon started (%s)\n", short_id(id))

			// The daemon writes its socket into the shared cache dir; wait for it
			// so `cld it`/`cld up` work right after install.
			if wait_daemon(ctx, c.SocketPath(), 30*time.Second) {
				fmt.Fprintln(cmd.ErrWriter, "cld: daemon is ready")
			} else {
				fmt.Fprintln(cmd.ErrWriter, "cld: daemon started but not answering yet; check `cld ls`")
			}

			// Point the user at authentication when nothing is configured yet, so a
			// fresh install has a clear next step. Authenticating is optional: a
			// session left unauthenticated simply prompts a login inside the
			// container, and that token stays in that container.
			if !auth_configured(c) {
				fmt.Fprintln(cmd.ErrWriter, "cld: sessions log in per container by default (cld remembers each "+
					"project's login). To instead share one Claude subscription across sessions, run "+
					"`cld auth login`, then `cld up --proxy` / `cld it --proxy` on the projects that should use it.")
			}
			return nil
		}),
	}
}

// auth_configured reports whether the daemon already has a broker login
// (`cld auth login`) — the only cross-session auth cld stores. It is optional:
// without it, sessions log in per container (and cld persists that login per
// project). When absent, install points the user at `cld auth login`.
func auth_configured(c *config.Config) bool {
	_, err := os.Stat(c.BrokerCredentialsPath())
	return err == nil
}

func short_id(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// wait_daemon polls the daemon's control socket until it answers or the timeout
// elapses.
func wait_daemon(ctx context.Context, socket string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := daemon.FetchInfo(ictx, socket)
		cancel()
		if err == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}
