package daemon

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

func TestDindNames(t *testing.T) {
	// One key, one engine, one network, one cache volume — all derivable, so a
	// teardown can find them without having recorded anything.
	require.Equal(t, "cld-api-dind", dind_container_name("cld-api"))
	require.Equal(t, "cld-api-dind-net", dind_network_name("cld-api"))
	require.Equal(t, "cld-api-dind-data", dind_volume_name("cld-api"))
	require.Equal(t, "tcp://cld-api-dind:2375", dind_endpoint("cld-api"))
}

func TestDindBinds(t *testing.T) {
	e := &entry{item: Item{LocalFolder: "/home/me/work/api", Workspace: "/workspace"}}

	// The workspace is bound at the path the devcontainer uses, so that
	// `docker run -v $(pwd):/app` — resolved by the engine, not by the
	// devcontainer — refers to the same files.
	require.Equal(t, []string{
		"cld-api-dind-data:/var/lib/docker",
		"/home/me/work/api:/workspace",
	}, dind_binds(e, "cld-api"))

	t.Run("skips the workspace when it is not resolved", func(t *testing.T) {
		require.Equal(t, []string{"cld-api-dind-data:/var/lib/docker"},
			dind_binds(&entry{}, "cld-api"))
	})
}

func TestDindMatches(t *testing.T) {
	binds := []string{"vol:/var/lib/docker", "/host:/workspace"}
	insp := func(image string, b []string) container_inspect {
		return container_inspect{
			Config:     &container.Config{Image: image},
			HostConfig: &container.HostConfig{Binds: b},
		}
	}

	require.True(t, dind_matches(insp("docker:dind", binds), "docker:dind", binds))

	t.Run("a changed image means replace", func(t *testing.T) {
		require.False(t, dind_matches(insp("docker:dind", binds), "docker:28-dind", binds))
	})
	t.Run("a changed workspace means replace", func(t *testing.T) {
		other := []string{"vol:/var/lib/docker", "/elsewhere:/workspace"}
		require.False(t, dind_matches(insp("docker:dind", other), "docker:dind", binds))
	})
	t.Run("a partial inspect is never a match", func(t *testing.T) {
		require.False(t, dind_matches(container_inspect{}, "docker:dind", binds))
	})
}

func TestDindEnv(t *testing.T) {
	t.Run("DOCKER_HOST is injected when an engine is up", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		e := &entry{docker_host: "tcp://cld-api-dind:2375"}

		env := d.session_env(e)
		require.Equal(t, "tcp://cld-api-dind:2375", envMap(env.Overrides())["DOCKER_HOST"])

		for _, v := range env.Vars {
			if v.Key == "DOCKER_HOST" {
				require.Equal(t, envOriginDocker, v.Origin)
			}
		}
	})

	t.Run("absent when there is no engine", func(t *testing.T) {
		// Including the case where one was asked for but failed to come up: a
		// session must not be pointed at an engine that is not there.
		d, _ := newTestDaemon(t)
		require.NotContains(t, envMap(d.session_env(&entry{}).Overrides()), "DOCKER_HOST")
	})

	t.Run("the user's own DOCKER_HOST wins", func(t *testing.T) {
		// cld's engine is a default, not a promise — pointing a project at a
		// real remote engine has to keep working.
		d, cfg := newTestDaemon(t)
		cfg.Env = config.EnvMap{"DOCKER_HOST": strp("tcp://build01:2376")}
		e := &entry{docker_host: "tcp://cld-api-dind:2375"}
		require.Equal(t, "tcp://build01:2376", envMap(d.session_env(e).Overrides())["DOCKER_HOST"])
	})
}

// TestDindDisabled pins that the feature costs nothing when it is off: no
// docker calls at all, which a nil client proves.
func TestDindDisabled(t *testing.T) {
	d, _ := newTestDaemon(t)
	require.Nil(t, d.cli)

	e := provision_entry("ctr")
	e.docker_host = "stale"
	d.ensure_dind(t.Context(), e, "ctr")
	require.Empty(t, e.docker_host, "a disabled engine must also clear a stale endpoint")
}

// TestDindLifecycle drives the whole thing against a real engine: the private
// network, the engine container, the devcontainer's attachment to it, reuse,
// replacement, and teardown.
func TestDindLifecycle(t *testing.T) {
	cli := require_docker(t)
	if testing.Short() {
		t.Skip("integration test; -short given")
	}

	id := run_container_labeled(t, cli, "", map[string]string{})

	cfg := &config.Config{CacheDir: t.TempDir(), DataDir: t.TempDir()}
	cfg.Docker = config.DockerConfig{Mode: config.DockerModeDind}
	d, err := New(cfg, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)

	e := provision_entry(id)
	key := d.backup_key(e)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		d.remove_dind(ctx, e)
		cli.VolumeRemove(ctx, dind_volume_name(key), client.VolumeRemoveOptions{Force: true})
	})

	// The engine image is a few hundred megabytes on a cold host.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	d.ensure_dind(ctx, e, id)

	require.Equal(t, dind_endpoint(key), e.docker_host,
		"an empty endpoint means the engine never became ready; see the warn log above")

	t.Run("the devcontainer can reach the engine", func(t *testing.T) {
		// The point of the whole design: a container cld did not create was
		// attached to a new network after the fact.
		out, code := in_container(t, d, id,
			"wget -qO- http://"+dind_container_name(key)+":2375/_ping")
		require.Equal(t, 0, code)
		require.Equal(t, "OK", strings.TrimSpace(out))
	})

	t.Run("the workspace is bound where the devcontainer sees it", func(t *testing.T) {
		engine, err := d.find_dind(ctx, key)
		require.NoError(t, err)
		insp, err := cli.ContainerInspect(ctx, engine, client.ContainerInspectOptions{})
		require.NoError(t, err)
		require.Contains(t, insp.Container.HostConfig.Binds, e.item.LocalFolder+":"+e.item.Workspace)
	})

	t.Run("a second reconcile reuses the engine", func(t *testing.T) {
		before, err := d.find_dind(ctx, key)
		require.NoError(t, err)

		d.ensure_dind(ctx, e, id)

		after, err := d.find_dind(ctx, key)
		require.NoError(t, err)
		require.Equal(t, before, after, "the engine must not be recreated on every reconcile")
		require.Equal(t, dind_endpoint(key), e.docker_host)
	})

	t.Run("a changed workspace replaces the engine", func(t *testing.T) {
		before, err := d.find_dind(ctx, key)
		require.NoError(t, err)

		e.item.Workspace = "/workspace2"
		d.ensure_dind(ctx, e, id)

		after, err := d.find_dind(ctx, key)
		require.NoError(t, err)
		require.NotEqual(t, before, after, "a bind cannot be changed in place")
		require.Equal(t, dind_endpoint(key), e.docker_host)
	})

	t.Run("teardown removes the engine and its network", func(t *testing.T) {
		d.remove_dind(ctx, e)

		ctrs, nets := d.dind_targets(ctx, key)
		require.Empty(t, ctrs)
		require.Empty(t, nets)

		// The image cache is kept: a plain down should not cost the next `cld
		// up` a re-pull of everything.
		res, err := cli.VolumeList(ctx, client.VolumeListOptions{})
		require.NoError(t, err)
		found := false
		for _, v := range res.Items {
			if v.Name == dind_volume_name(key) {
				found = true
			}
		}
		require.True(t, found, "the image cache volume must survive a teardown")
	})
}
