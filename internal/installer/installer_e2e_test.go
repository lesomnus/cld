package installer

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

func require_docker(t *testing.T) *client.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test; -short given")
	}
	cli, err := client.New(client.FromEnv)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		t.Skipf("docker not reachable: %v", err)
	}
	return cli
}

func TestInstallUninstall(t *testing.T) {
	cli := require_docker(t)

	// Start clean and always clean up (this uses the daemon container name).
	Uninstall(t.Context(), cli)
	t.Cleanup(func() { Uninstall(context.Background(), cli) })

	// A harmless stand-in for the real daemon image (no binds/env needed to
	// exercise create/find/remove).
	spec := Spec{Image: "alpine:3.20", Cmd: []string{"sleep", "3600"}, Restart: "no"}

	id, err := Install(t.Context(), cli, spec, false, io.Discard)
	require.NoError(t, err)
	require.NotEmpty(t, id)

	insp, err := cli.ContainerInspect(t.Context(), id, client.ContainerInspectOptions{})
	require.NoError(t, err)
	require.Equal(t, roleDaemon, insp.Container.Config.Labels[roleLabel])
	require.Equal(t, "/"+ContainerName, insp.Container.Name)

	ids, err := find(t.Context(), cli)
	require.NoError(t, err)
	require.Contains(t, ids, id)

	// A second install without --recreate is refused.
	_, err = Install(t.Context(), cli, spec, false, io.Discard)
	require.Error(t, err)

	// --recreate replaces the container.
	id2, err := Install(t.Context(), cli, spec, true, io.Discard)
	require.NoError(t, err)
	require.NotEqual(t, id, id2)

	removed, err := Uninstall(t.Context(), cli)
	require.NoError(t, err)
	require.True(t, removed)

	ids, err = find(t.Context(), cli)
	require.NoError(t, err)
	require.Empty(t, ids)
}

// An engine is a privileged container that exists only to serve cld's
// sessions, so uninstalling cld has to take it with it — otherwise removing
// cld leaves the sharpest thing it starts still running.
//
// Opt-in, because what it exercises is deliberately global: RemoveEngines drops
// EVERY engine on the host engine, which would knock over the daemon package's
// dind tests when `go test ./...` runs the two packages side by side.
func TestRemoveEngines(t *testing.T) {
	if os.Getenv("CLD_E2E_ENGINES") == "" {
		t.Skip("set CLD_E2E_ENGINES=1 to run; it removes every cld engine on this host")
	}

	cli := require_docker(t)
	ctx := t.Context()

	net, err := cli.NetworkCreate(ctx, "cld-test-engine-net", client.NetworkCreateOptions{
		Driver: "bridge",
		Labels: map[string]string{config.DockerLabel: "cld-test"},
	})
	require.NoError(t, err)

	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "cld-test-engine",
		Config: &container.Config{
			Image:  "alpine:3.20",
			Cmd:    []string{"sleep", "3600"},
			Labels: map[string]string{config.DockerLabel: "cld-test"},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{"cld-test-engine-net": {}},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		c := context.Background()
		cli.ContainerRemove(c, created.ID, client.ContainerRemoveOptions{Force: true})
		cli.NetworkRemove(c, net.ID, client.NetworkRemoveOptions{})
	})
	_, err = cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{})
	require.NoError(t, err)

	// Something else still attached to the network: a devcontainer normally is,
	// and Docker refuses to remove a network that has endpoints.
	other, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: "alpine:3.20", Cmd: []string{"sleep", "3600"}},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{"cld-test-engine-net": {}},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cli.ContainerRemove(context.Background(), other.ID, client.ContainerRemoveOptions{Force: true})
	})
	_, err = cli.ContainerStart(ctx, other.ID, client.ContainerStartOptions{})
	require.NoError(t, err)

	n, err := RemoveEngines(ctx, cli)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	sel := client.Filters{"label": {config.DockerLabel: true}}
	ctrs, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: sel})
	require.NoError(t, err)
	require.Empty(t, ctrs.Items, "the engine must be gone")

	nets, err := cli.NetworkList(ctx, client.NetworkListOptions{Filters: sel})
	require.NoError(t, err)
	require.Empty(t, nets.Items, "its network too, endpoints and all")

	// The bystander is left running: only cld's own resources are removed.
	insp, err := cli.ContainerInspect(ctx, other.ID, client.ContainerInspectOptions{})
	require.NoError(t, err)
	require.True(t, insp.Container.State.Running)

	t.Run("nothing to remove is not an error", func(t *testing.T) {
		n, err := RemoveEngines(ctx, cli)
		require.NoError(t, err)
		require.Zero(t, n)
	})
}
