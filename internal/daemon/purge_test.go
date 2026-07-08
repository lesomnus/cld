package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

// run_devcontainer_with_volume provisions a devcontainer like run_devcontainer
// but also mounts the named volume vol at /data, so a test can assert what a
// down keeps and a purge deletes.
func run_devcontainer_with_volume(t *testing.T, cli *client.Client, local_folder, vol string) string {
	t.Helper()

	created, err := cli.ContainerCreate(t.Context(), client.ContainerCreateOptions{
		Config: &container.Config{
			Image: test_image,
			Cmd:   []string{"sleep", "2147483647"},
			Labels: map[string]string{
				devc.LabelLocalFolder: local_folder,
				devc.LabelConfigFile:  filepath.Join(local_folder, ".devcontainer", "devcontainer.json"),
				devc.LabelMetadata:    `[{"remoteUser":"root"}]`,
			},
		},
		HostConfig: &container.HostConfig{
			Binds: []string{
				local_folder + ":/workspace",
				vol + ":/data",
			},
		},
	})
	require.NoError(t, err)

	id := created.ID
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	})

	_, err = cli.ContainerStart(t.Context(), id, client.ContainerStartOptions{})
	require.NoError(t, err)
	return id
}

func purge_test_config(tmp string, server_url string) *config.Config {
	return &config.Config{
		CacheDir: filepath.Join(tmp, "cache"),
		DataDir:  filepath.Join(tmp, "data"),
		Release: config.ReleaseConfig{
			BaseURL:       server_url,
			Channel:       "stable",
			CheckInterval: config.Duration(time.Hour),
		},
		Sync: config.SyncConfig{
			Debounce:         config.Duration(200 * time.Millisecond),
			FallbackInterval: config.Duration(time.Minute),
		},
	}
}

// TestPurge provisions a devcontainer with a named volume and a conversation
// backup, then verifies `cld purge` removes the container, deletes the named
// volume, and wipes the host-side conversation backup — the irreversible
// superset of down.
func TestPurge(t *testing.T) {
	cli := require_docker(t)
	pull_image(t, cli)

	server := fake_release(t, "9.9.9", []byte(fake_claude))

	cfg := purge_test_config(t.TempDir(), server.URL)
	d, err := New(cfg, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)
	d.self = build_cld(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go d.Run(ctx)

	t.Cleanup(func() {
		exec.Command("tmux", "-S", cfg.TmuxSocketPath(), "kill-server").Run()
	})

	wait_for(t, 10*time.Second, "daemon socket", func() bool {
		_, err := FetchItems(context.Background(), cfg.SocketPath())
		return err == nil
	})

	local_folder := fmt.Sprintf("/tmp/proj-purge-%d", time.Now().UnixNano())
	name := devc.DisplayName(local_folder)
	vol := fmt.Sprintf("cld-test-purge-vol-%d", time.Now().UnixNano())
	_, err = cli.VolumeCreate(t.Context(), client.VolumeCreateOptions{Name: vol})
	require.NoError(t, err)
	t.Cleanup(func() {
		cli.VolumeRemove(context.Background(), vol, client.VolumeRemoveOptions{Force: true})
	})

	ctr := run_devcontainer_with_volume(t, cli, local_folder, vol)

	wait_for(t, 60*time.Second, "item ready", func() bool {
		items, err := FetchItems(context.Background(), cfg.SocketPath())
		if err != nil {
			return false
		}
		it := find_item(items, name)
		return it != nil && it.Status == StatusReady
	})

	// Seed a conversation and wait for the sync loop to back it up on disk —
	// purge skips the final backup, so the backup must exist beforehand for the
	// deletion to be meaningful.
	_, code, err := dockerx.ExecOutput(t.Context(), cli, ctr, "", []string{
		"sh", "-c",
		`mkdir -p /root/.cld/claude/projects/-workspace` +
			` && echo '{"cwd":"/workspace"}' > /root/.cld/claude/projects/-workspace/s1.jsonl`,
	})
	require.NoError(t, err)
	require.Equal(t, 0, code)

	backupGlob := filepath.Join(cfg.DataDir, "projects", "*", "projects", "-workspace", "s1.jsonl")
	wait_for(t, 60*time.Second, "conversation backed up", func() bool {
		m, _ := filepath.Glob(backupGlob)
		return len(m) > 0
	})

	require.NoError(t, Purge(context.Background(), cfg.SocketPath(), name))

	// The entry is dropped from the listing.
	wait_for(t, 20*time.Second, "item gone", func() bool {
		items, err := FetchItems(context.Background(), cfg.SocketPath())
		return err == nil && find_item(items, name) == nil
	})

	// The container is really gone from the engine.
	_, err = cli.ContainerInspect(context.Background(), ctr, client.ContainerInspectOptions{})
	require.Error(t, err, "container should be removed by purge")

	// The named volume is deleted.
	_, err = cli.VolumeInspect(context.Background(), vol, client.VolumeInspectOptions{})
	require.Error(t, err, "named volume should be deleted by purge")

	// The host-side conversation backup is wiped.
	m, _ := filepath.Glob(backupGlob)
	require.Empty(t, m, "conversation backup should be deleted by purge")

	// purge on an unknown name is a clean 404, not a hang or a 500.
	err = Purge(context.Background(), cfg.SocketPath(), "no-such-devcontainer")
	require.Error(t, err)
}

// TestDownKeepsVolume is the counterpart to TestPurge: it pins that a plain
// `cld down` removes the container but leaves its named volume in place, so the
// data survives for a later `cld up`.
func TestDownKeepsVolume(t *testing.T) {
	cli := require_docker(t)
	pull_image(t, cli)

	server := fake_release(t, "9.9.9", []byte(fake_claude))

	cfg := purge_test_config(t.TempDir(), server.URL)
	d, err := New(cfg, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)
	d.self = build_cld(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go d.Run(ctx)

	t.Cleanup(func() {
		exec.Command("tmux", "-S", cfg.TmuxSocketPath(), "kill-server").Run()
	})

	wait_for(t, 10*time.Second, "daemon socket", func() bool {
		_, err := FetchItems(context.Background(), cfg.SocketPath())
		return err == nil
	})

	local_folder := fmt.Sprintf("/tmp/proj-downvol-%d", time.Now().UnixNano())
	name := devc.DisplayName(local_folder)
	vol := fmt.Sprintf("cld-test-downvol-%d", time.Now().UnixNano())
	_, err = cli.VolumeCreate(t.Context(), client.VolumeCreateOptions{Name: vol})
	require.NoError(t, err)
	t.Cleanup(func() {
		cli.VolumeRemove(context.Background(), vol, client.VolumeRemoveOptions{Force: true})
	})

	ctr := run_devcontainer_with_volume(t, cli, local_folder, vol)

	wait_for(t, 60*time.Second, "item ready", func() bool {
		items, err := FetchItems(context.Background(), cfg.SocketPath())
		if err != nil {
			return false
		}
		it := find_item(items, name)
		return it != nil && it.Status == StatusReady
	})

	require.NoError(t, Down(context.Background(), cfg.SocketPath(), name))

	wait_for(t, 20*time.Second, "item gone", func() bool {
		items, err := FetchItems(context.Background(), cfg.SocketPath())
		return err == nil && find_item(items, name) == nil
	})

	// The container is gone, but its named volume survives the down.
	_, err = cli.ContainerInspect(context.Background(), ctr, client.ContainerInspectOptions{})
	require.Error(t, err, "container should be removed by down")
	_, err = cli.VolumeInspect(context.Background(), vol, client.VolumeInspectOptions{})
	require.NoError(t, err, "named volume must survive down")
}
