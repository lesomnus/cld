package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/lesomnus/cld/internal/devc"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// composeProjectLabel groups the containers and networks the devcontainer CLI
// creates for a Docker Compose based devcontainer.
const composeProjectLabel = "com.docker.compose.project"

// down takes a final backup, then stops and removes the devcontainer, keeping
// its named volumes and the host-side conversation backup so a later `cld up`
// restores the history. It is teardown without the purge.
func (d *Daemon) down(ctx context.Context, e *entry) error {
	return d.remove_devcontainer(ctx, e, false)
}

// purge stops and removes the devcontainer and everything that outlives a plain
// down: its named (and anonymous) volumes and its whole host-side backup dir
// (conversations and the project's own settings snapshot), so no trace is left
// on the engine or on disk. Because that history is being destroyed, purge
// skips the final backup a down takes.
func (d *Daemon) purge(ctx context.Context, e *entry) error {
	return d.remove_devcontainer(ctx, e, true)
}

// remove_devcontainer stops and removes the devcontainer and drops its entry.
// For a Compose-based devcontainer the whole project is removed (the dev service plus
// any sidecars such as the docker-in-docker container) so no orphans are left
// behind — except a sibling explicitly marked cld.ignore, which is spared.
//
// When purge is false (`cld down`) the named volumes and the host-side backup
// are kept, and a final backup runs first so the history survives. When purge is
// true (`cld purge`) the devcontainer's named and anonymous volumes are removed
// and its backup dir is deleted, and the final backup is skipped since that
// history is being destroyed anyway.
//
// It runs on the entry's worker goroutine (posted from the API handler), which
// serializes it with the sync loop and guarantees the copy-out completes while
// the container still exists — a destroy event alone cannot back up a container
// Docker has already removed.
func (d *Daemon) remove_devcontainer(ctx context.Context, e *entry, purge bool) error {
	// Final backup while the container is still present: copy_out docker-cp's
	// out of it, so it must run before removal. A purge deletes that backup, so
	// it does not bother taking one.
	if !purge && e.item.Workspace != "" {
		d.copy_out(ctx, e, dirty{settings: true, project: true})
	}
	if e.watch_stop != nil {
		e.watch_stop()
		e.watch_stop = nil
	}
	if e.item.Name != "" {
		d.tmux.KillSession(ctx, devc.SessionName(e.item.Name))
	}

	containers, networks := d.down_targets(ctx, e.id)

	// Record the named volumes while the containers that hold them still exist —
	// an in-use volume cannot be removed, so this only collects the names now and
	// deletes them once the containers are gone.
	var volumes []string
	if purge {
		volumes = d.purge_volumes(ctx, containers)
	}

	var errs []error
	for _, id := range containers {
		// Never tear down the daemon's own container.
		if id == d.self_ctr {
			continue
		}
		// RemoveVolumes drops the container's anonymous volumes; named ones are
		// removed by name below (Docker never removes named volumes implicitly).
		if _, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: purge}); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", short(id), err))
		}
	}
	// Best-effort: drop the project's networks once its containers are gone. A
	// network still in use (or pre-existing/external) simply stays.
	for _, id := range networks {
		d.cli.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
	}

	if purge {
		for _, name := range volumes {
			if _, err := d.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true}); err != nil {
				errs = append(errs, fmt.Errorf("remove volume %s: %w", name, err))
			}
		}
		// Wipe this project's whole host-side backup dir: conversations and
		// its own settings snapshot. Nothing here is shared with another
		// project's backup, so this cannot affect any other container.
		if dir := d.cfg.ProjectBackupDir(d.backup_key(e)); dir != "" {
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, fmt.Errorf("remove backup %s: %w", dir, err))
			}
		}
	}

	d.sessions.clear(e.id)
	d.remove(e)
	verb := "removed"
	if purge {
		verb = "purged"
	}
	d.log.Info(verb, slog.String("id", short(e.id)), slog.String("name", e.item.Name))
	return errors.Join(errs...)
}

// purge_volumes lists the named volumes a purge must delete: every named volume
// mounted into the containers being removed. It is gathered before those
// containers are removed, because an in-use volume cannot be deleted. The
// daemon's own container is not in the removal set and a cld.ignore sibling was
// already dropped by down_targets, so neither's volumes are ever collected — the
// opt-out is honored for volumes too. Anonymous volumes are not listed here;
// they are dropped by RemoveVolumes when their container is removed.
func (d *Daemon) purge_volumes(ctx context.Context, containers []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range containers {
		if id == d.self_ctr {
			continue
		}
		insp, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			continue
		}
		for _, m := range insp.Container.Mounts {
			if m.Type != mount.TypeVolume || m.Name == "" || seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			out = append(out, m.Name)
		}
	}
	return out
}

// managed_devcontainer re-applies ensure's own gate to the live container: it
// is a cld-managed devcontainer only if it still carries the devcontainer
// local-folder label and is not excluded by the cld.ignore label or an ignore
// glob (mirroring ensure.go). It inspects at call time, so this is authoritative
// against current reality rather than trusting the tracking set — which is a
// superset: an entry is created for every started container and only later
// declassified by ensure, and a non-running container is never classified at
// all. down --all uses this so it never removes a container that is not (or no
// longer) a devcontainer cld manages: a plain sidecar, a container labelled
// cld.ignore after it was provisioned, or one that has already vanished all
// report false and are left alone.
func (d *Daemon) managed_devcontainer(ctx context.Context, id string) bool {
	insp, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return false
	}
	labels := map[string]string{}
	if insp.Container.Config != nil {
		labels = insp.Container.Config.Labels
	}
	folder := labels[devc.LabelLocalFolder]
	return folder != "" && !devc.Ignored(labels, folder, d.cfg.Ignore)
}

// down_targets resolves what to remove for the devcontainer identified by id.
// For a Compose project it returns every container that shares the project
// label plus the project's networks; for a plain container it returns just that
// container and no networks. A sibling a user explicitly marked cld.ignore is
// spared from the sweep so the opt-out is honored even inside a managed project
// (id itself is never a match — an ignored container is never a tracked
// devcontainer and so never reaches here as the target).
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
			if c.ID != id && devc.Ignored(c.Labels, c.Labels[devc.LabelLocalFolder], d.cfg.Ignore) {
				continue
			}
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
