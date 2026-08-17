package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// dind gives a project's claude session a Docker engine of its own: a
// docker-in-docker container on a private bridge network the devcontainer is
// attached to, with DOCKER_HOST pointed at it.
//
// cld cannot add a mount to a running container — mounts are fixed at creation
// — but it CAN attach a network to one, which is what makes this possible for a
// container cld did not create (one VS Code started, say).
//
// Enabling it hands the session root on a privileged container that shares the
// host kernel. That is the user's decision to make; see docs/session-docker.md.

// dindLabel marks the resources cld owns for a project, valued with the
// project key. The engine carries no devcontainer.local_folder label, so the
// daemon never mistakes it for a devcontainer to provision.
const dindLabel = "cld.dind"

// dindReadyTimeout bounds the wait for the engine to accept commands. It is
// best-effort: exceeding it leaves the session without DOCKER_HOST rather than
// failing provisioning.
const dindReadyTimeout = 30 * time.Second

func dind_container_name(key string) string { return key + "-dind" }
func dind_network_name(key string) string   { return key + "-dind-net" }
func dind_volume_name(key string) string    { return key + "-dind-data" }

// dind_endpoint is what DOCKER_HOST is set to inside the devcontainer. The
// engine's full container name is used as the host rather than a short alias
// like "docker": the devcontainer is usually attached to other networks too,
// and a short name could resolve to something else entirely.
func dind_endpoint(key string) string {
	return fmt.Sprintf("tcp://%s:%d", dind_container_name(key), config.DockerEnginePort)
}

// ensure_dind brings up the project's engine and attaches the devcontainer to
// it, recording the endpoint on the entry for session_env to inject. It is
// idempotent: an engine that already runs is reused, and a container left over
// from a previous configuration is replaced.
//
// Best-effort. When anything fails the endpoint is left empty, so the session
// comes up without DOCKER_HOST instead of pointing claude at an engine that is
// not there — a state `cld setting env` shows plainly.
func (d *Daemon) ensure_dind(ctx context.Context, e *entry, id string) {
	cfg := d.cfg.DockerFor(e.item.LocalFolder)
	e.docker_host = ""
	if !cfg.Enabled() {
		return
	}

	key := d.backup_key(e)
	log := d.log.With(slog.String("name", e.item.Name), slog.String("key", key))

	net, err := d.ensure_dind_network(ctx, key)
	if err != nil {
		log.Warn("dind: network failed", slog.String("error", err.Error()))
		return
	}
	engine, err := d.ensure_dind_engine(ctx, e, key, net, cfg.Image)
	if err != nil {
		log.Warn("dind: engine failed", slog.String("error", err.Error()))
		return
	}
	// Attaching the devcontainer is the step that could not be done at all if
	// networks, like mounts, were fixed at creation.
	if err := d.attach_network(ctx, net, id); err != nil {
		log.Warn("dind: attach failed", slog.String("error", err.Error()))
		return
	}
	if err := d.wait_dind(ctx, engine); err != nil {
		log.Warn("dind: engine did not become ready", slog.String("error", err.Error()))
		return
	}

	e.docker_host = dind_endpoint(key)
	log.Info("dind ready", slog.String("endpoint", e.docker_host))
}

// ensure_dind_network returns the project's private network, creating it if
// needed. Two containers ever join it: the engine and the devcontainer.
func (d *Daemon) ensure_dind_network(ctx context.Context, key string) (string, error) {
	name := dind_network_name(key)
	res, err := d.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: client.Filters{"label": {dindLabel + "=" + key: true}},
	})
	if err != nil {
		return "", err
	}
	for _, n := range res.Items {
		if n.Name == name {
			return name, nil
		}
	}

	_, err = d.cli.NetworkCreate(ctx, name, client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: map[string]string{dindLabel: key},
	})
	// A concurrent reconcile may have won the race; that is a success here.
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return "", err
	}
	return name, nil
}

// ensure_dind_engine returns the running engine container for the project. An
// existing container is reused, started if stopped, and replaced when the
// configuration it was created from no longer matches.
func (d *Daemon) ensure_dind_engine(ctx context.Context, e *entry, key, net, image string) (string, error) {
	name := dind_container_name(key)
	binds := dind_binds(e, key)

	if id, err := d.find_dind(ctx, key); err != nil {
		return "", err
	} else if id != "" {
		insp, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return "", err
		}
		if dind_matches(insp.Container, image, binds) {
			if insp.Container.State == nil || !insp.Container.State.Running {
				if _, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
					return "", fmt.Errorf("start engine: %w", err)
				}
			}
			return id, nil
		}
		// The image or the workspace changed: a container cannot be
		// reconfigured in place, so replace it. The image cache lives in a
		// named volume, which survives this.
		if _, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
			return "", fmt.Errorf("replace engine: %w", err)
		}
	}

	if err := d.pull_if_missing(ctx, image); err != nil {
		return "", err
	}

	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:  image,
			Labels: map[string]string{dindLabel: key},
			// An empty DOCKER_TLS_CERTDIR is how the official image is told to
			// serve the plain port instead of generating TLS material. The
			// network holds only this engine and its devcontainer.
			Env: []string{"DOCKER_TLS_CERTDIR="},
		},
		HostConfig: &container.HostConfig{
			// docker-in-docker needs it. This is the sharpest edge of the
			// feature and the reason it is opt-in.
			Privileged: true,
			Binds:      binds,
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{net: {}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create engine: %w", err)
	}
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start engine: %w", err)
	}
	return created.ID, nil
}

// dind_binds is the engine's storage plus the workspace.
//
// The workspace bind is what makes `docker run -v $(pwd):/app` behave. A path
// in a `docker run` is resolved by the ENGINE, so inside a dind it means a path
// in the engine container, not in the devcontainer — without this the mount
// would silently be an empty directory. Binding the host workspace at the same
// path the devcontainer sees makes the two agree.
func dind_binds(e *entry, key string) []string {
	binds := []string{dind_volume_name(key) + ":/var/lib/docker"}
	if e.item.LocalFolder != "" && e.item.Workspace != "" {
		binds = append(binds, e.item.LocalFolder+":"+e.item.Workspace)
	}
	return binds
}

// dind_matches reports whether an existing engine was created from the same
// configuration, since neither an image nor a bind can be changed in place.
func dind_matches(c container_inspect, image string, binds []string) bool {
	if c.Config == nil || c.Config.Image != image {
		return false
	}
	if c.HostConfig == nil {
		return false
	}
	if len(c.HostConfig.Binds) != len(binds) {
		return false
	}
	for i, b := range binds {
		if c.HostConfig.Binds[i] != b {
			return false
		}
	}
	return true
}

// find_dind returns the project's engine container id, running or not, or ""
// when there is none.
func (d *Daemon) find_dind(ctx context.Context, key string) (string, error) {
	res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{"label": {dindLabel + "=" + key: true}},
	})
	if err != nil {
		return "", err
	}
	if len(res.Items) == 0 {
		return "", nil
	}
	return res.Items[0].ID, nil
}

// attach_network connects the devcontainer to the project's network. Already
// being attached is the normal case on a re-reconcile, not an error.
func (d *Daemon) attach_network(ctx context.Context, net, id string) error {
	_, err := d.cli.NetworkConnect(ctx, net, client.NetworkConnectOptions{Container: id})
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, "already exists") || strings.Contains(msg, "already connected") {
		return nil
	}
	return err
}

// wait_dind waits until the engine answers, by asking it from inside its own
// container — the daemon is not on the project's network and has no other way
// to reach it.
func (d *Daemon) wait_dind(ctx context.Context, engine string) error {
	ctx, cancel := context.WithTimeout(ctx, dindReadyTimeout)
	defer cancel()

	var last string
	for {
		out, code, err := dockerx.ExecOutput(ctx, d.cli, engine, "", []string{
			"docker", "info", "--format", "{{.ServerVersion}}",
		})
		if err == nil && code == 0 {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = strings.TrimSpace(out)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("engine not ready: %s", last)
		case <-time.After(time.Second):
		}
	}
}

// dind_targets lists the engine container and network cld owns for a project,
// so a teardown removes them alongside the devcontainer they belong to. The
// image cache volume is not listed: it is mounted into the engine container, so
// a purge collects it the same way it collects a devcontainer's own volumes —
// and a plain `cld down` keeps it, leaving the cache for the next `cld up`.
func (d *Daemon) dind_targets(ctx context.Context, key string) (containers []string, networks []string) {
	sel := client.Filters{"label": {dindLabel + "=" + key: true}}
	if res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: sel}); err == nil {
		for _, c := range res.Items {
			containers = append(containers, c.ID)
		}
	}
	if res, err := d.cli.NetworkList(ctx, client.NetworkListOptions{Filters: sel}); err == nil {
		for _, n := range res.Items {
			networks = append(networks, n.ID)
		}
	}
	return containers, networks
}

// remove_dind drops a project's engine and network, keeping its image cache.
// It backs the destroy path: a devcontainer removed outside cld (a plain
// `docker rm`) would otherwise leave a privileged engine running for a project
// that no longer exists. Best-effort — there may be nothing to remove.
func (d *Daemon) remove_dind(ctx context.Context, e *entry) {
	if e.item.LocalFolder == "" {
		return // never resolved far enough to have an engine
	}
	containers, networks := d.dind_targets(ctx, d.backup_key(e))
	for _, id := range containers {
		if _, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
			d.log.Warn("dind: remove engine",
				slog.String("name", e.item.Name), slog.String("error", err.Error()))
		}
	}
	for _, id := range networks {
		d.remove_dind_network(ctx, id)
	}
	if len(containers) > 0 {
		d.log.Info("dind removed", slog.String("name", e.item.Name))
	}
}

// remove_dind_network detaches whatever is still on the network before removing
// it. Docker refuses to remove a network that has endpoints, and the
// devcontainer is normally still running here — this path also serves turning
// the engine off for a project, not just tearing the project down. Removing
// only cld's own network makes the force-disconnect safe: nothing else was ever
// meant to be on it.
func (d *Daemon) remove_dind_network(ctx context.Context, id string) {
	if insp, err := d.cli.NetworkInspect(ctx, id, client.NetworkInspectOptions{}); err == nil {
		for ctr := range insp.Network.Containers {
			d.cli.NetworkDisconnect(ctx, id, client.NetworkDisconnectOptions{Container: ctr, Force: true})
		}
	}
	if _, err := d.cli.NetworkRemove(ctx, id, client.NetworkRemoveOptions{}); err != nil {
		d.log.Warn("dind: remove network", slog.String("error", err.Error()))
	}
}

// pull_if_missing fetches the engine image when the host does not have it yet.
func (d *Daemon) pull_if_missing(ctx context.Context, image string) error {
	if _, err := d.cli.ImageInspect(ctx, image); err == nil {
		return nil
	}
	d.log.Info("dind: pulling engine image", slog.String("image", image))
	res, err := d.cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", image, err)
	}
	io.Copy(io.Discard, res)
	werr := res.Wait(ctx)
	res.Close()
	if werr != nil {
		return fmt.Errorf("pull %s: %w", image, werr)
	}
	return nil
}
