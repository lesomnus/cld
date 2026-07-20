// Package installer runs the cld daemon (`cld serve`) as a container on the
// host's own Docker engine. It is the counterpart of the reference
// docker-compose.yaml kept at the repo root: SpecFor is the single source of
// truth for that deployment, and the compose file mirrors it (guarded by a
// drift test).
package installer

import (
	"context"
	"fmt"
	"io"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/devcup"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// ContainerName is the daemon container's name; roleLabel marks it for
// discovery independent of the name.
const (
	ContainerName = "cld"
	roleLabel     = "cld.role"
	roleDaemon    = "daemon"
)

// Spec is the daemon container: `cld serve` on the host's Docker, with the
// shared cache/data dirs and the Docker socket mounted. It mirrors the `cld`
// service in docker-compose.yaml.
type Spec struct {
	Image   string
	Cmd     []string
	Env     []string
	Binds   []string
	Restart string
}

// SpecFor builds the daemon spec. dockerHost is $DOCKER_HOST (how the daemon
// will reach Docker); cacheDir/dataDir are the host's cld cache and data dirs
// (shared with the daemon so the host `cld` reaches the same socket, and the
// conversation backups persist); home is the host user's home, mounted
// read-only so the daemon can read host-side files like ~/.dotfiles; uid/gid
// own those dirs.
func SpecFor(image string, dockerHost string, cacheDir, dataDir, home string, uid, gid int) (Spec, error) {
	access, err := devcup.AccessFor(dockerHost)
	if err != nil {
		return Spec{}, err
	}
	// The daemon shares the host's cache/data dirs over bind mounts (that is how
	// the host `cld` reaches its socket), so the engine must be local. A remote
	// (tcp) DOCKER_HOST would resolve those binds on the engine's host instead.
	if access.Bind == "" {
		return Spec{}, fmt.Errorf("cld install needs a local Docker engine; " +
			"DOCKER_HOST points at a remote one — run the daemon on the engine's host " +
			"with `cld install` or `docker compose up -d` there")
	}

	env := []string{
		"XDG_CACHE_HOME=/data/cache",
		"XDG_DATA_HOME=/data/share",
		"DOCKER_HOST=unix:///var/run/docker.sock",
		config.HostHomeEnv + "=" + config.HostHomeMount,
		fmt.Sprintf("PUID=%d", uid),
		fmt.Sprintf("PGID=%d", gid),
	}
	binds := []string{
		cacheDir + ":/data/cache/cld",
		dataDir + ":/data/share/cld",
		// The host home, read-only, so the daemon can read ~/.dotfiles even
		// though it runs inside a container.
		home + ":" + config.HostHomeMount + ":ro",
		access.Bind,
	}

	return Spec{
		Image:   image,
		Cmd:     []string{"serve"},
		Env:     env,
		Binds:   binds,
		Restart: string(container.RestartPolicyUnlessStopped),
	}, nil
}

// Install creates and starts the daemon container, pulling the image if absent.
// An existing daemon is an error unless recreate is set, in which case it is
// replaced. It returns the new container id.
func Install(ctx context.Context, cli *client.Client, spec Spec, recreate bool, out io.Writer) (string, error) {
	existing, err := find(ctx, cli)
	if err != nil {
		return "", err
	}
	if len(existing) > 0 {
		if !recreate {
			return "", fmt.Errorf("cld daemon already installed; use --recreate to replace it")
		}
		if err := remove(ctx, cli, existing); err != nil {
			return "", err
		}
	}

	if _, err := cli.ImageInspect(ctx, spec.Image); err != nil {
		fmt.Fprintf(out, "pulling %s...\n", spec.Image)
		res, err := cli.ImagePull(ctx, spec.Image, client.ImagePullOptions{})
		if err != nil {
			return "", fmt.Errorf("pull %s: %w", spec.Image, err)
		}
		io.Copy(io.Discard, res)
		werr := res.Wait(ctx)
		res.Close()
		if werr != nil {
			return "", fmt.Errorf("pull %s: %w", spec.Image, werr)
		}
	}

	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: ContainerName,
		Config: &container.Config{
			Image:  spec.Image,
			Cmd:    spec.Cmd,
			Env:    spec.Env,
			Labels: map[string]string{roleLabel: roleDaemon},
		},
		HostConfig: &container.HostConfig{
			Binds:         spec.Binds,
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(spec.Restart)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create daemon: %w", err)
	}
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start daemon: %w", err)
	}
	return created.ID, nil
}

// Uninstall stops and removes the daemon container. removed is false (no error)
// when there was none.
func Uninstall(ctx context.Context, cli *client.Client) (removed bool, err error) {
	ids, err := find(ctx, cli)
	if err != nil || len(ids) == 0 {
		return false, err
	}
	return true, remove(ctx, cli, ids)
}

// find returns the ids of daemon containers (by role label), running or not.
func find(ctx context.Context, cli *client.Client) ([]string, error) {
	res, err := cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{"label": {roleLabel + "=" + roleDaemon: true}},
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Items))
	for _, c := range res.Items {
		ids = append(ids, c.ID)
	}
	return ids, nil
}

func remove(ctx context.Context, cli *client.Client, ids []string) error {
	for _, id := range ids {
		if _, err := cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("remove %s: %w", id, err)
		}
	}
	return nil
}
