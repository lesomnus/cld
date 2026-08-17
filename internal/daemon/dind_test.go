package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	own := config.DockerConfig{Mode: config.DockerModeDind, Scope: config.DockerScopeProject}
	shared := config.DockerConfig{Mode: config.DockerModeDind, Scope: config.DockerScopeShared}

	t.Run("an engine of its own gets the workspace", func(t *testing.T) {
		// Bound at the path the devcontainer uses, so that `docker run -v
		// $(pwd):/app` — resolved by the engine, not by the devcontainer —
		// refers to the same files.
		require.Equal(t, []string{
			"cld-api-dind-data:/var/lib/docker",
			"/home/me/work/api:/workspace",
		}, dind_binds(e, "cld-api", own))
	})

	t.Run("the shared engine gets no workspace", func(t *testing.T) {
		// It cannot: mounts are fixed at creation, so every new project would
		// mean recreating the engine and killing what the others are running.
		require.Equal(t, []string{"cld-dind-data:/var/lib/docker"},
			dind_binds(e, "cld", shared))
	})

	t.Run("skips the workspace when it is not resolved", func(t *testing.T) {
		require.Equal(t, []string{"cld-api-dind-data:/var/lib/docker"},
			dind_binds(&entry{}, "cld-api", own))
	})
}

// A bind is resolved by the HOST engine, so a "~/x" the daemon passed through
// unchanged would have Docker create a directory literally named "~" — looking
// for all the world like it worked.
func TestExpandDindVolumes(t *testing.T) {
	const home = "/home/me"

	t.Run("expands both spellings, in the source only", func(t *testing.T) {
		got, err := expand_dind_volumes(&config.DindService{Volumes: []string{
			"~/certs:/certs",
			"${HOME}/workspaces:/workspaces",
			"${HOME}:/home-ro:ro",
			"~:/h",
		}}, home)
		require.NoError(t, err)
		require.Equal(t, []string{
			"/home/me/certs:/certs",
			"/home/me/workspaces:/workspaces",
			"/home/me:/home-ro:ro",
			"/home/me:/h",
		}, got.Volumes)
	})

	t.Run("leaves an absolute source alone", func(t *testing.T) {
		got, err := expand_dind_volumes(&config.DindService{
			Volumes: []string{"/srv/cache:/cache"},
		}, home)
		require.NoError(t, err)
		require.Equal(t, []string{"/srv/cache:/cache"}, got.Volumes)
	})

	t.Run("does not touch the target", func(t *testing.T) {
		// Only the source names a host path; the target is inside the engine.
		got, err := expand_dind_volumes(&config.DindService{
			Volumes: []string{"/srv/x:/opt/${HOME}"},
		}, home)
		require.NoError(t, err)
		require.Equal(t, []string{"/srv/x:/opt/${HOME}"}, got.Volumes)
	})

	t.Run("fails when the host home is unknown", func(t *testing.T) {
		// Better than creating a directory named "~" on the host.
		_, err := expand_dind_volumes(&config.DindService{
			Volumes: []string{"~/certs:/certs"},
		}, "")
		require.ErrorContains(t, err, "cannot tell what")
	})

	t.Run("nothing to do", func(t *testing.T) {
		got, err := expand_dind_volumes(nil, home)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("the original is left untouched", func(t *testing.T) {
		// It is the loaded config; another project reads the same one.
		in := &config.DindService{Volumes: []string{"~/a:/a"}}
		_, err := expand_dind_volumes(in, home)
		require.NoError(t, err)
		require.Equal(t, []string{"~/a:/a"}, in.Volumes)
	})
}

func TestDindKey(t *testing.T) {
	d, cfg := newTestDaemon(t)
	e := &entry{item: Item{LocalFolder: "/work/api"}}

	t.Run("shared by default", func(t *testing.T) {
		key := d.dind_key(e, cfg.DockerFor(e.item.LocalFolder))
		require.Equal(t, dindSharedKey, key)
		require.Equal(t, "cld-dind", dind_container_name(key))
	})

	t.Run("a project key never collides with the shared one", func(t *testing.T) {
		// backup_key always prefixes "cld-", including for a project called cld.
		e := &entry{dev_name: "cld", item: Item{LocalFolder: "/work/cld"}}
		require.NotEqual(t, dindSharedKey, d.backup_key(e))
	})

	t.Run("its own when the project asks", func(t *testing.T) {
		cfg.Projects = []config.ProjectConfig{{
			Match:  config.StringList{"/work/**"},
			Docker: config.DockerConfig{Scope: config.DockerScopeProject},
		}}
		key := d.dind_key(e, cfg.DockerFor(e.item.LocalFolder))
		require.Equal(t, d.backup_key(e), key)
	})
}

// A single project's teardown must never take the shared engine with it: the
// others are still using it, and its value is the cache they all filled.
func TestProjectDindKey(t *testing.T) {
	d, cfg := newTestDaemon(t)
	e := &entry{item: Item{LocalFolder: "/work/api"}}

	require.Empty(t, d.project_dind_key(e), "shared engine: nothing for this project to remove")

	cfg.Docker = config.DockerConfig{Scope: config.DockerScopeProject}
	require.Equal(t, d.backup_key(e), d.project_dind_key(e))

	cfg.Docker = config.DockerConfig{Mode: config.DockerModeOff}
	require.Empty(t, d.project_dind_key(e))

	cfg.Docker = config.DockerConfig{Scope: config.DockerScopeProject}
	require.Empty(t, d.project_dind_key(&entry{}), "an unresolved container has no engine")
}

func TestDindSpec(t *testing.T) {
	e := &entry{item: Item{LocalFolder: "/host/api", Workspace: "/workspace"}}
	cfg := config.DockerConfig{
		Mode:  config.DockerModeDind,
		Scope: config.DockerScopeProject,
		Image: "docker:dind",
	}
	hash := func(c config.DockerConfig, over *config.DindService) string {
		return dind_spec_hash(dind_create_options(e, "cld-api", "net", c, over))
	}

	base := hash(cfg, nil)

	t.Run("is stable", func(t *testing.T) {
		require.Equal(t, base, hash(cfg, nil))
	})

	t.Run("changes with the image", func(t *testing.T) {
		other := cfg
		other.Image = "docker:28-dind"
		require.NotEqual(t, base, hash(other, nil))
	})

	t.Run("changes with the workspace of an engine that binds it", func(t *testing.T) {
		moved := &entry{item: Item{LocalFolder: "/host/api", Workspace: "/elsewhere"}}
		require.NotEqual(t, base,
			dind_spec_hash(dind_create_options(moved, "cld-api", "net", cfg, nil)))
	})

	t.Run("the shared engine is the same whichever project asks", func(t *testing.T) {
		// The invariant the whole arrangement rests on: every project must
		// resolve the SAME spec, or each would rebuild the engine the others
		// are using — on every reconcile, forever.
		shared := config.DockerConfig{Mode: config.DockerModeDind, Image: "docker:dind"}
		want := dind_spec_hash(dind_create_options(e, dindSharedKey, "net", shared, nil))

		for _, other := range []*entry{
			{item: Item{LocalFolder: "/host/other", Workspace: "/w2"}},
			{dev_name: "svc", user: "root", home: "/root",
				item: Item{Name: "svc", LocalFolder: "/elsewhere/svc", Workspace: "/src"}},
			{}, // not resolved at all
		} {
			opts := dind_create_options(other, dindSharedKey, "net", shared, nil)
			require.Equal(t, want, dind_spec_hash(opts),
				"a project must not reshape the engine it shares")
			require.Equal(t, "cld-dind", opts.Name)
		}
	})

	t.Run("changes with any override", func(t *testing.T) {
		// An edited override has to produce a new engine: a container cannot be
		// reconfigured in place.
		require.NotEqual(t, base, hash(cfg, &config.DindService{Volumes: []string{"/a:/b"}}))
		require.NotEqual(t, base, hash(cfg, &config.DindService{Command: config.StringList{"--mtu=1400"}}))
		require.NotEqual(t, base, hash(cfg, &config.DindService{Cpus: 2}))
	})

	t.Run("does not change with map ordering", func(t *testing.T) {
		over := &config.DindService{Environment: config.EnvList{
			"A": "1", "B": "2", "C": "3", "D": "4", "E": "5",
		}}
		first := hash(cfg, over)
		for range 20 {
			require.Equal(t, first, hash(cfg, over), "map iteration order must not rebuild the engine")
		}
	})

	t.Run("an engine is matched by its recorded spec", func(t *testing.T) {
		c := container_inspect{Config: &container.Config{
			Labels: map[string]string{dindSpecLabel: base},
		}}
		require.True(t, dind_matches(c, base))
		require.False(t, dind_matches(c, "other"))
		require.False(t, dind_matches(container_inspect{}, base))
	})
}

func TestDindOverrideApplies(t *testing.T) {
	e := &entry{item: Item{LocalFolder: "/host/api", Workspace: "/workspace"}}
	// Project scope, so the workspace bind is in the base the override adds to.
	cfg := config.DockerConfig{
		Mode:  config.DockerModeDind,
		Scope: config.DockerScopeProject,
		Image: "docker:dind",
	}
	yes := true
	no := false

	t.Run("lists append to what cld set", func(t *testing.T) {
		// An extra volume must not cost the engine its own storage.
		opts := dind_create_options(e, "cld-api", "net", cfg, &config.DindService{
			Volumes: []string{"/host/certs:/etc/docker/certs.d:ro"},
			CapAdd:  []string{"SYS_ADMIN"},
		})
		require.Equal(t, []string{
			"cld-api-dind-data:/var/lib/docker",
			"/host/api:/workspace",
			"/host/certs:/etc/docker/certs.d:ro",
		}, opts.HostConfig.Binds)
		require.Equal(t, []string{"SYS_ADMIN"}, opts.HostConfig.CapAdd)
	})

	t.Run("scalars replace", func(t *testing.T) {
		opts := dind_create_options(e, "cld-api", "net", cfg, &config.DindService{
			Image:      "docker:dind-rootless",
			Privileged: &no,
			Command:    config.StringList{"--insecure-registry", "reg:5000"},
			MemLimit:   "4g",
			Cpus:       2.5,
		})
		require.Equal(t, "docker:dind-rootless", opts.Config.Image)
		require.False(t, opts.HostConfig.Privileged)
		require.Equal(t, []string{"--insecure-registry", "reg:5000"}, []string(opts.Config.Cmd))
		require.EqualValues(t, 4*1024*1024*1024, opts.HostConfig.Memory)
		require.EqualValues(t, 2.5e9, opts.HostConfig.NanoCPUs)

		back := dind_create_options(e, "cld-api", "net", cfg, &config.DindService{Privileged: &yes})
		require.True(t, back.HostConfig.Privileged)
	})

	t.Run("maps merge, and cld's own labels survive", func(t *testing.T) {
		opts := dind_create_options(e, "cld-api", "net", cfg, &config.DindService{
			Environment: config.EnvList{"HTTP_PROXY": "http://proxy:3128"},
			Labels:      map[string]string{"team": "infra"},
			Sysctls:     map[string]string{"net.ipv4.ip_forward": "1"},
		})
		require.Contains(t, opts.Config.Env, "DOCKER_TLS_CERTDIR=")
		require.Contains(t, opts.Config.Env, "HTTP_PROXY=http://proxy:3128")
		require.Equal(t, "infra", opts.Config.Labels["team"])
		require.Equal(t, "cld-api", opts.Config.Labels[dindLabel],
			"an override must not orphan the engine by dropping the label cleanup finds it by")
		require.Equal(t, "1", opts.HostConfig.Sysctls["net.ipv4.ip_forward"])
	})

	t.Run("devices and ports", func(t *testing.T) {
		opts := dind_create_options(e, "cld-api", "net", cfg, &config.DindService{
			Devices: []string{"/dev/fuse", "/dev/net/tun:/dev/tun:rw"},
			Ports:   []string{"12375:2375"},
		})
		require.Equal(t, "/dev/fuse", opts.HostConfig.Devices[0].PathOnHost)
		require.Equal(t, "/dev/fuse", opts.HostConfig.Devices[0].PathInContainer)
		require.Equal(t, "rwm", opts.HostConfig.Devices[0].CgroupPermissions)
		require.Equal(t, "/dev/tun", opts.HostConfig.Devices[1].PathInContainer)
		require.Equal(t, "rw", opts.HostConfig.Devices[1].CgroupPermissions)

		port, _, ok := parse_port("12375:2375")
		require.True(t, ok)
		require.Len(t, opts.HostConfig.PortBindings[port], 1)
		require.Equal(t, "12375", opts.HostConfig.PortBindings[port][0].HostPort)
	})

	t.Run("nil override changes nothing", func(t *testing.T) {
		require.Equal(t,
			dind_spec_hash(dind_create_options(e, "cld-api", "net", cfg, nil)),
			dind_spec_hash(dind_create_options(e, "cld-api", "net", cfg, &config.DindService{})))
	})
}

func TestParsePort(t *testing.T) {
	for _, tc := range []struct{ in, port, host string }{
		{"2375", "2375/tcp", ""},
		{"12375:2375", "2375/tcp", "12375"},
		{"127.0.0.1:12375:2375", "2375/tcp", "12375"},
		{"5353:53/udp", "53/udp", "5353"},
	} {
		port, binding, ok := parse_port(tc.in)
		require.True(t, ok, tc.in)
		require.Equal(t, tc.port, port.String(), tc.in)
		require.Equal(t, tc.host, binding.HostPort, tc.in)
	}

	for _, in := range []string{"", "a:b", "1:2:3:4", "notaport"} {
		_, _, ok := parse_port(in)
		require.False(t, ok, in)
	}
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

// TestDindDisabled pins that opting out costs nothing: no docker calls at all,
// which a nil client proves.
func TestDindDisabled(t *testing.T) {
	d, cfg := newTestDaemon(t)
	require.Nil(t, d.cli)
	cfg.Docker = config.DockerConfig{Mode: config.DockerModeOff}

	e := provision_entry("ctr")
	e.docker_host = "stale"
	d.ensure_dind(t.Context(), e, "ctr")
	require.Empty(t, e.docker_host, "a disabled engine must also clear a stale endpoint")
}

// An engine is on by default, so a project that says nothing gets one. This is
// the assertion that fails first if the default is ever flipped back.
func TestDindEnabledByDefault(t *testing.T) {
	// Deliberately not newTestDaemon, which turns the engine off the way every
	// other test wants it: this is the assertion about the shipped default.
	got := (&config.Config{}).DockerFor("/work/api")
	require.True(t, got.Enabled())
	require.True(t, got.Shared())
	require.Equal(t, config.DockerImageDefault, got.Image)
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

	// File-backed, so the override beside cld.yaml is exercised end to end.
	// Project scope, so this test owns the engine it creates instead of racing
	// another test over the shared one.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cld.yaml"),
		[]byte("docker: {mode: dind, scope: project}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, config.DindFileName), []byte(`
services:
  dind:
    labels:
      team: infra
    mem_limit: 512m
`), 0o644))

	cfg, err := config.ReadFromFile(filepath.Join(dir, "cld.yaml"))
	require.NoError(t, err)
	require.NoError(t, cfg.Evaluate())
	cfg.CacheDir, cfg.DataDir = t.TempDir(), t.TempDir()

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

	t.Run("the override reached the engine", func(t *testing.T) {
		engine, err := d.find_dind(ctx, key)
		require.NoError(t, err)
		insp, err := cli.ContainerInspect(ctx, engine, client.ContainerInspectOptions{})
		require.NoError(t, err)
		require.Equal(t, "infra", insp.Container.Config.Labels["team"])
		require.EqualValues(t, 512*1024*1024, insp.Container.HostConfig.Memory)
	})

	t.Run("the API points cld docker at the engine", func(t *testing.T) {
		rr := httptest.NewRecorder()
		e.mbox = new_mailbox()
		go e.mbox.run()
		t.Cleanup(e.mbox.close)
		e.publish()
		d.entries = map[string]*entry{e.id: e}
		d.handle_get_engine(rr, httptest.NewRequest(http.MethodGet, "/docker/engine?name="+e.item.Name, nil))
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

		var got DockerEngine
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
		require.Equal(t, dind_container_name(key), got.Name)
		require.Equal(t, dind_endpoint(key), got.Endpoint)
		require.True(t, got.Running)

		// The id is what `cld docker` execs into, so it must be the engine.
		engine, err := d.find_dind(ctx, key)
		require.NoError(t, err)
		require.Equal(t, engine, got.Container)
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

	t.Run("an edited override replaces the engine", func(t *testing.T) {
		// A container cannot be reconfigured in place, so the spec hash has to
		// notice and rebuild.
		before, err := d.find_dind(ctx, key)
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(filepath.Join(dir, config.DindFileName), []byte(`
services:
  dind:
    labels:
      team: platform
`), 0o644))
		d.ensure_dind(ctx, e, id)

		after, err := d.find_dind(ctx, key)
		require.NoError(t, err)
		require.NotEqual(t, before, after)

		insp, err := cli.ContainerInspect(ctx, after, client.ContainerInspectOptions{})
		require.NoError(t, err)
		require.Equal(t, "platform", insp.Container.Config.Labels["team"])
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

// TestDindShared is the default arrangement: one engine, every project on it,
// one build cache. What has to hold is that two projects land on the SAME
// engine and that neither can take it away from the other.
func TestDindShared(t *testing.T) {
	cli := require_docker(t)
	if testing.Short() {
		t.Skip("integration test; -short given")
	}

	cfg := &config.Config{CacheDir: t.TempDir(), DataDir: t.TempDir()}
	require.NoError(t, cfg.Evaluate())
	require.True(t, cfg.DockerFor("/work/a").Shared(), "shared is the default")

	d, err := New(cfg, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		d.remove_shared_dind(c, true)
	})

	// Two devcontainers, two different projects.
	id_a := run_container_labeled(t, cli, "", map[string]string{})
	id_b := run_container_labeled(t, cli, "", map[string]string{})

	a := provision_entry(id_a)
	a.item.LocalFolder, a.item.Name = "/work/a", "alpha"
	b := provision_entry(id_b)
	b.item.LocalFolder, b.item.Name = "/work/b", "bravo"

	d.ensure_dind(ctx, a, id_a)
	d.ensure_dind(ctx, b, id_b)

	t.Run("both projects point at the one engine", func(t *testing.T) {
		want := dind_endpoint(dindSharedKey)
		require.Equal(t, want, a.docker_host)
		require.Equal(t, want, b.docker_host, "a second project must reuse, not rebuild")

		ctrs, _ := d.dind_targets(ctx, dindSharedKey)
		require.Len(t, ctrs, 1, "one engine, not one per project")
	})

	t.Run("both devcontainers can reach it", func(t *testing.T) {
		for _, id := range []string{id_a, id_b} {
			out, code := in_container(t, d, id,
				"wget -qO- http://"+dind_container_name(dindSharedKey)+":2375/_ping")
			require.Equal(t, 0, code)
			require.Equal(t, "OK", strings.TrimSpace(out))
		}
	})

	t.Run("the shared engine has no workspace bind", func(t *testing.T) {
		// It cannot have one: adding a project would mean recreating the engine
		// and killing whatever the others were building.
		engine, err := d.find_dind(ctx, dindSharedKey)
		require.NoError(t, err)
		insp, err := cli.ContainerInspect(ctx, engine, client.ContainerInspectOptions{})
		require.NoError(t, err)
		require.Equal(t, []string{dind_volume_name(dindSharedKey) + ":/var/lib/docker"},
			insp.Container.HostConfig.Binds)
	})

	t.Run("one project's teardown leaves it for the others", func(t *testing.T) {
		d.remove_dind(ctx, a)

		ctrs, _ := d.dind_targets(ctx, dindSharedKey)
		require.Len(t, ctrs, 1, "bravo is still using it")

		out, code := in_container(t, d, id_b,
			"wget -qO- http://"+dind_container_name(dindSharedKey)+":2375/_ping")
		require.Equal(t, 0, code)
		require.Equal(t, "OK", strings.TrimSpace(out))
	})

	t.Run("removing the last of them takes the engine", func(t *testing.T) {
		d.remove_shared_dind(ctx, false)

		ctrs, nets := d.dind_targets(ctx, dindSharedKey)
		require.Empty(t, ctrs)
		require.Empty(t, nets)

		// Not a purge, so the cache the projects filled is still there.
		res, err := cli.VolumeList(ctx, client.VolumeListOptions{})
		require.NoError(t, err)
		found := false
		for _, v := range res.Items {
			if v.Name == dind_volume_name(dindSharedKey) {
				found = true
			}
		}
		require.True(t, found, "a down must not throw away the build cache")
	})
}
