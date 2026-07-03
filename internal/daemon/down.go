package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lesomnus/cld/internal/devc"
	"github.com/moby/moby/client"
)

// composeProjectLabel groups the containers and networks the devcontainer CLI
// creates for a Docker Compose based devcontainer.
const composeProjectLabel = "com.docker.compose.project"

// down takes a final backup, then stops and removes the devcontainer and drops
// its entry. For a Compose-based devcontainer the whole project is removed (the
// dev service plus any sidecars such as the docker-in-docker container) so no
// orphans are left behind. Named volumes and the host-side conversation backup
// are kept, so a later `cld up` restores the history.
//
// It runs on the entry's worker goroutine (posted from the API handler), which
// serializes it with the sync loop and guarantees the copy-out completes while
// the container still exists — a destroy event alone cannot back up a container
// Docker has already removed.
func (d *Daemon) down(ctx context.Context, e *entry) error {
	// Final backup while the container is still present: copy_out docker-cp's
	// out of it, so it must run before removal.
	if e.item.Workspace != "" {
		d.copy_out(ctx, e, dirty{global: true, project: true})
	}
	if e.watch_stop != nil {
		e.watch_stop()
		e.watch_stop = nil
	}
	if e.item.Name != "" {
		d.tmux.KillSession(ctx, devc.SessionName(e.item.Name))
	}

	containers, networks := d.down_targets(ctx, e.id)

	var errs []error
	for _, id := range containers {
		// Never tear down the daemon's own container.
		if id == d.self_ctr {
			continue
		}
		if _, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", short(id), err))
		}
	}
	// Best-effort: drop the project's networks once its containers are gone. A
	// network still in use (or pre-existing/external) simply stays.
	for _, id := range networks {
		d.cli.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
	}

	d.sessions.clear(e.id)
	d.remove(e)
	d.log.Info("removed", slog.String("id", short(e.id)), slog.String("name", e.item.Name))
	return errors.Join(errs...)
}

// down_targets resolves what to remove for the devcontainer identified by id.
// For a Compose project it returns every container that shares the project
// label plus the project's networks; for a plain container it returns just that
// container and no networks.
func (d *Daemon) down_targets(ctx context.Context, id string) (containers []string, networks []string) {
	insp, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		// The container may already be gone; fall back to removing it by id.
		return []string{id}, nil
	}
	labels := map[string]string{}
	if insp.Container.Config != nil {
		labels = insp.Container.Config.Labels
	}
	project := labels[composeProjectLabel]
	if project == "" {
		return []string{id}, nil
	}

	sel := client.Filters{"label": {composeProjectLabel + "=" + project: true}}
	if res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: sel}); err == nil {
		for _, c := range res.Items {
			containers = append(containers, c.ID)
		}
	}
	if len(containers) == 0 {
		// The list failed or raced with removal; still remove the one we know.
		containers = []string{id}
	}
	if res, err := d.cli.NetworkList(ctx, client.NetworkListOptions{Filters: sel}); err == nil {
		for _, n := range res.Items {
			networks = append(networks, n.ID)
		}
	}
	return containers, networks
}
