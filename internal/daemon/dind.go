package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/go-units"
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

// dindSpecLabel records the hash of the spec an engine was created from, so a
// changed image, workspace or override is noticed on the next reconcile.
const dindSpecLabel = "cld.dind.spec"

// dindReadyTimeout bounds the wait for the engine to accept commands. It is
// best-effort: exceeding it leaves the session without DOCKER_HOST rather than
// failing provisioning.
const dindReadyTimeout = 30 * time.Second

// dindSharedKey names the engine every project shares by default. A project key
// is always "cld-<something>" (see backup_key), so a bare "cld" can never
// collide with one — not even for a project actually called cld.
const dindSharedKey = "cld"

// dind_key is the key naming the engine, network and cache a container uses:
// the shared one, or one of its own when the project asked for that.
func (d *Daemon) dind_key(e *entry, cfg config.DockerConfig) string {
	if cfg.Shared() {
		return dindSharedKey
	}
	return d.backup_key(e)
}

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

	key := d.dind_key(e, cfg)
	log := d.log.With(slog.String("name", e.item.Name), slog.String("engine", key))

	net, err := d.ensure_dind_network(ctx, key)
	if err != nil {
		log.Warn("dind: network failed", slog.String("error", err.Error()))
		return
	}
	over, err := d.cfg.LoadDindOverride(e.item.LocalFolder)
	if err != nil {
		// The user wrote an override cld could not read; running the engine
		// without it would apply settings they think are in effect.
		log.Warn("dind: override failed", slog.String("error", err.Error()))
		return
	}
	engine, err := d.ensure_dind_engine(ctx, e, key, net, cfg, over)
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
// existing container is reused, started if stopped, and replaced when the spec
// it was created from no longer matches — a container cannot be reconfigured in
// place, so an edited override means a new one.
func (d *Daemon) ensure_dind_engine(ctx context.Context, e *entry, key, net string, cfg config.DockerConfig, over *config.DindService) (string, error) {
	opts := dind_create_options(e, key, net, cfg, over)
	sum := dind_spec_hash(opts)
	opts.Config.Labels[dindSpecLabel] = sum

	if id, err := d.find_dind(ctx, key); err != nil {
		return "", err
	} else if id != "" {
		insp, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return "", err
		}
		if dind_matches(insp.Container, sum) {
			if insp.Container.State == nil || !insp.Container.State.Running {
				if _, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
					return "", fmt.Errorf("start engine: %w", err)
				}
			}
			return id, nil
		}
		// The image cache lives in a named volume, which survives the replacement.
		if _, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true}); err != nil {
			return "", fmt.Errorf("replace engine: %w", err)
		}
	}

	if err := d.pull_if_missing(ctx, opts.Config.Image); err != nil {
		return "", err
	}

	created, err := d.cli.ContainerCreate(ctx, opts)
	if err != nil {
		return "", fmt.Errorf("create engine: %w", err)
	}
	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("start engine: %w", err)
	}
	return created.ID, nil
}

// dind_create_options is cld's own engine definition with the user's override
// applied over it, if there is one.
func dind_create_options(e *entry, key, net string, cfg config.DockerConfig, over *config.DindService) client.ContainerCreateOptions {
	opts := client.ContainerCreateOptions{
		Name: dind_container_name(key),
		Config: &container.Config{
			Image:  cfg.Image,
			Labels: map[string]string{dindLabel: key},
			// An empty DOCKER_TLS_CERTDIR is how the official image is told to
			// serve the plain port instead of generating TLS material. Nothing
			// but cld's own devcontainers is ever on the network.
			Env: []string{"DOCKER_TLS_CERTDIR="},
		},
		HostConfig: &container.HostConfig{
			// docker-in-docker needs it. This is the sharpest edge of the
			// feature: the session is root on a privileged container that
			// shares the host kernel.
			Privileged: true,
			Binds:      dind_binds(e, key, cfg),
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{net: {}},
		},
	}
	apply_dind_override(&opts, over)
	return opts
}

// apply_dind_override folds the user's compose-shaped override into the engine
// definition: scalars replace, maps merge key by key, and lists append to what
// cld set — so an extra volume or capability adds to the engine instead of
// replacing its storage or its privileges. A nil override changes nothing.
//
// Anything the override cannot express was rejected when the file was read, so
// there is nothing to silently drop here.
func apply_dind_override(opts *client.ContainerCreateOptions, o *config.DindService) {
	if o == nil {
		return
	}
	cfg, host := opts.Config, opts.HostConfig

	if o.Image != "" {
		cfg.Image = o.Image
	}
	if len(o.Command) > 0 {
		cfg.Cmd = o.Command
	}
	if len(o.Entrypoint) > 0 {
		cfg.Entrypoint = o.Entrypoint
	}
	for _, k := range sorted_keys(o.Environment) {
		cfg.Env = append(cfg.Env, k+"="+o.Environment[k])
	}
	for _, k := range sorted_keys(o.Labels) {
		cfg.Labels[k] = o.Labels[k]
	}

	host.Binds = append(host.Binds, o.Volumes...)
	if o.Privileged != nil {
		host.Privileged = *o.Privileged
	}
	host.CapAdd = append(host.CapAdd, o.CapAdd...)
	host.CapDrop = append(host.CapDrop, o.CapDrop...)
	host.SecurityOpt = append(host.SecurityOpt, o.SecurityOpt...)
	host.ExtraHosts = append(host.ExtraHosts, o.ExtraHosts...)
	for _, addr := range o.Dns {
		if ip, err := netip.ParseAddr(addr); err == nil {
			host.DNS = append(host.DNS, ip)
		}
	}
	if len(o.Sysctls) > 0 && host.Sysctls == nil {
		host.Sysctls = map[string]string{}
	}
	for k, v := range o.Sysctls {
		host.Sysctls[k] = v
	}
	for _, dev := range o.Devices {
		host.Devices = append(host.Devices, device_mapping(dev))
	}
	for _, spec := range o.Ports {
		port, binding, ok := parse_port(spec)
		if !ok {
			continue // rejected when the file was read
		}
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = network.PortSet{}
		}
		cfg.ExposedPorts[port] = struct{}{}
		if host.PortBindings == nil {
			host.PortBindings = network.PortMap{}
		}
		host.PortBindings[port] = append(host.PortBindings[port], binding)
	}
	if o.MemLimit != "" {
		if n, err := units.RAMInBytes(o.MemLimit); err == nil {
			host.Memory = n
		}
	}
	if o.Cpus > 0 {
		host.NanoCPUs = int64(o.Cpus * 1e9)
	}
}

// parse_port reads a compose port mapping: "HOST:CONTAINER[/proto]", or a bare
// "CONTAINER[/proto]" for an ephemeral host port. The engine has no TLS and no
// auth, so publishing it is a deliberate act — cld does not do it by default.
func parse_port(spec string) (network.Port, network.PortBinding, bool) {
	proto := "tcp"
	if s, p, ok := strings.Cut(spec, "/"); ok {
		spec, proto = s, p
	}

	host_ip, host_port, container := "", "", spec
	switch parts := strings.Split(spec, ":"); len(parts) {
	case 1:
	case 2:
		host_port, container = parts[0], parts[1]
	case 3:
		host_ip, host_port, container = parts[0], parts[1], parts[2]
	default:
		return network.Port{}, network.PortBinding{}, false
	}

	num, err := strconv.ParseUint(container, 10, 16)
	if err != nil {
		return network.Port{}, network.PortBinding{}, false
	}
	port, err := network.ParsePort(fmt.Sprintf("%d/%s", num, proto))
	if err != nil {
		return network.Port{}, network.PortBinding{}, false
	}

	binding := network.PortBinding{HostPort: host_port}
	if host_ip != "" {
		ip, err := netip.ParseAddr(host_ip)
		if err != nil {
			return network.Port{}, network.PortBinding{}, false
		}
		binding.HostIP = ip
	}
	return port, binding, true
}

// device_mapping parses a compose device string, "host[:container[:perms]]".
func device_mapping(s string) container.DeviceMapping {
	parts := strings.SplitN(s, ":", 3)
	m := container.DeviceMapping{PathOnHost: parts[0], PathInContainer: parts[0], CgroupPermissions: "rwm"}
	if len(parts) > 1 && parts[1] != "" {
		m.PathInContainer = parts[1]
	}
	if len(parts) > 2 && parts[2] != "" {
		m.CgroupPermissions = parts[2]
	}
	return m
}

// sorted_keys keeps generated environment and labels deterministic, so the
// spec hash does not change with map iteration order and rebuild the engine
// for no reason.
func sorted_keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dind_binds is the engine's storage, plus the workspace for an engine that
// belongs to a single project.
//
// The workspace bind is what makes `docker run -v $(pwd):/app` behave: a path
// in a `docker run` is resolved by the ENGINE, so inside a dind it names a path
// in the engine container, not in the devcontainer, and without the bind the
// mount would silently be an empty directory. Binding the host workspace at the
// path the devcontainer uses makes the two agree.
//
// A SHARED engine cannot have it. Mounts are fixed when a container is created,
// so every new project would mean recreating the engine — killing whatever the
// other projects were running in it. Builds do not need it (a build context is
// streamed by the client), which is what makes sharing worth the trade; a
// project that needs the bind sets `docker: {scope: project}`.
func dind_binds(e *entry, key string, cfg config.DockerConfig) []string {
	binds := []string{dind_volume_name(key) + ":/var/lib/docker"}
	if cfg.Shared() {
		return binds
	}
	if e.item.LocalFolder != "" && e.item.Workspace != "" {
		binds = append(binds, e.item.LocalFolder+":"+e.item.Workspace)
	}
	return binds
}

// dind_matches reports whether an existing engine was created from the same
// spec. The spec is hashed into a label at creation, so this covers everything
// that goes into the container — image, binds, and every field of an override —
// rather than the handful of things worth comparing by hand.
func dind_matches(c container_inspect, sum string) bool {
	if c.Config == nil {
		return false
	}
	return c.Config.Labels[dindSpecLabel] == sum
}

// dind_spec_hash digests the create options. It is deliberately over the whole
// thing: an override edited in any way should produce a new engine, and
// forgetting to add a newly supported field here would silently keep the old
// container.
func dind_spec_hash(opts client.ContainerCreateOptions) string {
	b, err := json.Marshal(struct {
		Config     *container.Config     `json:"config"`
		HostConfig *container.HostConfig `json:"host_config"`
	}{opts.Config, opts.HostConfig})
	if err != nil {
		// Cannot happen for these types; a constant would make every engine
		// look unchanged, so prefer a value that forces a rebuild.
		return "unhashable"
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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

// dind_running reports whether an engine container is up. A stopped one is
// still worth reporting to `cld docker`, which explains it rather than failing
// with a bare exec error.
func (d *Daemon) dind_running(ctx context.Context, id string) bool {
	insp, err := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return false
	}
	return insp.Container.State != nil && insp.Container.State.Running
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

// project_dind_key is the key of an engine that exists only for this project,
// or "" when it uses the shared one (or none) — i.e. the key a teardown of this
// project alone may remove.
func (d *Daemon) project_dind_key(e *entry) string {
	if e.item.LocalFolder == "" {
		return "" // never resolved far enough to have an engine
	}
	cfg := d.cfg.DockerFor(e.item.LocalFolder)
	if !cfg.Enabled() || cfg.Shared() {
		return ""
	}
	return d.backup_key(e)
}

// remove_shared_dind drops the engine every project shares, with its network.
// It is for the moments when there is nothing left to share it: the last
// devcontainer going away (`cld down --all`), or cld itself being removed.
// purge also deletes the accumulated image and build cache.
func (d *Daemon) remove_shared_dind(ctx context.Context, purge bool) {
	containers, networks := d.dind_targets(ctx, dindSharedKey)
	for _, id := range containers {
		if _, err := d.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: purge}); err != nil {
			d.log.Warn("dind: remove shared engine", slog.String("error", err.Error()))
		}
	}
	for _, id := range networks {
		d.remove_dind_network(ctx, id)
	}
	if purge {
		name := dind_volume_name(dindSharedKey)
		if _, err := d.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true}); err != nil {
			d.log.Warn("dind: remove shared cache", slog.String("error", err.Error()))
		}
	}
	if len(containers) > 0 {
		d.log.Info("shared dind removed", slog.Bool("purged", purge))
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
	key := d.project_dind_key(e)
	if key == "" {
		return // no engine of its own; the shared one outlives this project
	}
	containers, networks := d.dind_targets(ctx, key)
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
