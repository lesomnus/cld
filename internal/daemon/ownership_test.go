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
	"github.com/stretchr/testify/require"
)

// nonroot_image ships a `node` user (uid 1000, home /home/node) that exists in
// the image, so the daemon can resolve and run as it with no race.
const nonroot_image = "node:22-alpine"

// TestRestoreOwnsProjectsForNonRootUser is the regression test for the config
// tree ownership bug. docker cp (used by the backup restore) creates any
// intermediate directory it is not given explicitly — projects/, projects/<enc>/
// — as root, while the files inside carry the real uid. claude runs as the
// container's unprivileged user, so a root-owned projects/<enc>/ let it resume
// a conversation (read) but not start a new one (write a new transcript), which
// looked like claude dying the instant you created a new conversation.
//
// Every other integration test runs the container as root, where the bug is
// invisible; this one uses a non-root user, provisions with a restore, and
// asserts the user can create a new transcript.
func TestRestoreOwnsProjectsForNonRootUser(t *testing.T) {
	cli := require_docker(t)
	pull_ref(t, cli, nonroot_image)

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

	folder := fmt.Sprintf("/tmp/proj-own-%d", time.Now().UnixNano())
	name := devc.DisplayName(folder)
	const cfgDir = "/home/node/.cld/claude"
	const enc = "-workspace" // EncodeProjectPath("/workspace")

	ready := func(what string) {
		wait_for(t, 90*time.Second, what, func() bool {
			items, err := FetchItems(context.Background(), cfg.SocketPath())
			if err != nil {
				return false
			}
			it := find_item(items, name)
			return it != nil && it.Status == StatusReady
		})
	}

	// First generation: provision as the node user, then seed a conversation
	// transcript so there is something to back up.
	c1 := run_devcontainer_as(t, cli, folder, nonroot_image, "node")
	ready("c1 ready")

	_, code, err := dockerx.ExecOutput(t.Context(), cli, c1, "node", []string{
		"sh", "-c", fmt.Sprintf(
			"mkdir -p %s/projects/%s && echo '{\"cwd\":\"/workspace\"}' > %s/projects/%s/s1.jsonl",
			cfgDir, enc, cfgDir, enc),
	})
	require.NoError(t, err)
	require.Equal(t, 0, code)

	// Back it up and tear the container down.
	require.NoError(t, Down(context.Background(), cfg.SocketPath(), name))
	wait_for(t, 20*time.Second, "c1 gone", func() bool {
		items, err := FetchItems(context.Background(), cfg.SocketPath())
		return err == nil && find_item(items, name) == nil
	})

	// Second generation: same folder -> same backup key -> the restore runs,
	// which is where docker cp would leave projects/<enc> owned by root.
	c2 := run_devcontainer_as(t, cli, folder, nonroot_image, "node")
	ready("c2 ready")

	// The restore brought the transcript back...
	_, ok, err := dockerx.ReadFile(t.Context(), cli, c2, cfgDir+"/projects/"+enc+"/s1.jsonl")
	require.NoError(t, err)
	require.True(t, ok, "restored transcript should be present")

	// ...and the node user can create a NEW conversation's transcript in the
	// restored directory. Before the fix this failed with EACCES because
	// projects/<enc> was owned by root.
	out, code, err := dockerx.ExecOutput(t.Context(), cli, c2, "node", []string{
		"sh", "-c", fmt.Sprintf("touch %s/projects/%s/new-conversation.jsonl", cfgDir, enc),
	})
	require.NoError(t, err)
	require.Equalf(t, 0, code, "node must be able to create a new transcript in projects/%s: %s", enc, out)
}
