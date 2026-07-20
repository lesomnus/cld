package cmd

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/otx/log"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/z"
	"github.com/moby/moby/client"

	"os/signal"
)

func NewCmdServe() *xli.Command {
	return &xli.Command{
		Name:  "serve",
		Brief: "watch docker events and provision devcontainers with claude",
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			// The daemon runs only inside a container: it reaches Docker through
			// a mounted socket and reads the host home through a read-only mount
			// (see `cld install` / docker-compose.yaml), neither of which holds
			// when run bare on the host. Refuse rather than half-work.
			if !insideContainer() {
				return errors.New("cld serve runs the daemon inside a container; " +
					"run `cld install` to launch it (or `docker compose up -d`) — " +
					"it no longer runs directly on the host")
			}

			cli, err := client.New(client.FromEnv)
			if err != nil {
				return z.Err(err, "docker client")
			}
			defer cli.Close()

			d, err := daemon.New(c, cli, log.From(ctx))
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()

			err = d.Run(ctx)
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}),
	}
}

// insideContainer reports whether this process is running inside a container.
// It is a cheap, local check (no Docker call): the CLD_HOST_HOME marker that
// `cld install`/compose set, the runtime marker files Docker and Podman drop at
// the filesystem root, and finally the init process's cgroup for other runtimes.
func insideContainer() bool {
	if os.Getenv(config.HostHomeEnv) != "" || os.Getenv("CLD_SELF_CONTAINER") != "" {
		return true
	}
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(data)
		for _, marker := range []string{"docker", "containerd", "kubepods", "libpod", "/lxc"} {
			if strings.Contains(s, marker) {
				return true
			}
		}
	}
	return false
}
