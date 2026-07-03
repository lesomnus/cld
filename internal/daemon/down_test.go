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
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

// TestDown provisions a devcontainer, then verifies `cld down` removes the
// container from the engine and drops its entry while keeping the host-side
// conversation backup.
func TestDown(t *testing.T) {
	cli := require_docker(t)
	pull_image(t, cli)

	server := fake_release(t, "9.9.9", []byte(fake_claude))

	tmp := t.TempDir()
	cfg := &config.Config{
		CacheDir: filepath.Join(tmp, "cache"),
		DataDir:  filepath.Join(tmp, "data"),
		Release: config.ReleaseConfig{
			BaseURL:       server.URL,
			Channel:       "stable",
			CheckInterval: config.Duration(time.Hour),
		},
		Sync: config.SyncConfig{
			Debounce:         config.Duration(200 * time.Millisecond),
			FallbackInterval: config.Duration(time.Minute),
		},
	}

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

	local_folder := fmt.Sprintf("/tmp/proj-down-%d", time.Now().UnixNano())
	name := devc.DisplayName(local_folder)
	ctr := run_devcontainer(t, cli, local_folder)

	wait_for(t, 60*time.Second, "item ready", func() bool {
		items, err := FetchItems(context.Background(), cfg.SocketPath())
		if err != nil {
			return false
		}
		it := find_item(items, name)
		return it != nil && it.Status == StatusReady
	})

	// Seed a conversation so there is something to back up on the way down.
	_, code, err := dockerx.ExecOutput(t.Context(), cli, ctr, "", []string{
		"sh", "-c",
		`mkdir -p /root/.cld/claude/projects/-workspace` +
			` && echo '{"cwd":"/workspace"}' > /root/.cld/claude/projects/-workspace/s1.jsonl`,
	})
	require.NoError(t, err)
	require.Equal(t, 0, code)

	require.NoError(t, Down(context.Background(), cfg.SocketPath(), name))

	// The entry is dropped from the listing.
	wait_for(t, 20*time.Second, "item gone", func() bool {
		items, err := FetchItems(context.Background(), cfg.SocketPath())
		return err == nil && find_item(items, name) == nil
	})

	// The container is really gone from the engine.
	_, err = cli.ContainerInspect(context.Background(), ctr, client.ContainerInspectOptions{})
	require.Error(t, err, "container should be removed by down")

	// The final backup ran before removal, so the conversation survived.
	matches, _ := filepath.Glob(filepath.Join(cfg.DataDir, "projects", "*", "projects", "-workspace", "s1.jsonl"))
	require.NotEmpty(t, matches, "conversation backup should be kept after down")
	data, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	require.Contains(t, string(data), `"cwd":"/workspace"`)

	// down on an unknown name is a clean 404, not a hang or a 500.
	err = Down(context.Background(), cfg.SocketPath(), "no-such-devcontainer")
	require.Error(t, err)
}
