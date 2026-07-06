package cmd

import (
	"context"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/termx"
)

// attachTo hands the terminal over to a devcontainer's claude session; it is
// shared by `cld it` and `cld up`. It names the outer window after the
// devcontainer, then picks the attach route that fits the deployment:
//
//   - docker-exec into the daemon's container when this host can see it (the
//     host needs no tmux);
//   - an API-relayed pty when the daemon is only reachable through the
//     in-container relay (e.g. we ARE a managed container);
//   - a local tmux attach when the daemon runs on this host.
//
// It does not return on success — the attach replaces the process or exits.
func attachTo(ctx context.Context, c *config.Config, name string) error {
	// The daemon's tmux runs with set-titles off, so it leaves this outer title
	// alone; naming it after the devcontainer keeps multiple attach windows
	// distinguishable.
	termx.SetTitle(name)

	session := devc.SessionName(name)

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
}
