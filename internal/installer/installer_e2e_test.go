package installer

import (
	"context"
	"io"
	"testing"
	"time"

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
