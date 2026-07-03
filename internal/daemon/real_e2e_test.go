package daemon

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/lesomnus/cld/internal/release"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

// TestRealClaudeInstall downloads the real Claude Code binary from the
// official release channel, installs it into a container, and runs it. It
// verifies the parts that the fake-binary integration test cannot: the live
// channel/manifest/checksum path and that the real binary actually executes
// on a stock Linux image. It does not exercise interactive auth (that needs
// real credentials). Gated behind CLD_E2E_REAL because it hits the network.
func TestRealClaudeInstall(t *testing.T) {
	if os.Getenv("CLD_E2E_REAL") == "" {
		t.Skip("set CLD_E2E_REAL=1 to run the real-download E2E test")
	}
	cli := require_docker(t)

	const img = "debian:12-slim"
	ctx := t.Context()
	res, err := cli.ImagePull(ctx, img, client.ImagePullOptions{})
	require.NoError(t, err)
	drain(res)

	rc := release.NewClient("https://downloads.claude.ai/claude-code-releases")
	cache := &release.Cache{Dir: t.TempDir(), Client: rc}
	mgr := &release.Manager{Client: rc, Cache: cache, Channel: "stable"}

	platform, err := release.PlatformFor(runtime.GOARCH, false)
	require.NoError(t, err)

	version, bin, err := mgr.Ensure(ctx, platform)
	require.NoError(t, err)
	t.Logf("resolved and verified claude %s for %s", version, platform)

	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: img, Cmd: []string{"sleep", "600"}},
	})
	require.NoError(t, err)
	ctr := created.ID
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cli.ContainerRemove(c, ctr, client.ContainerRemoveOptions{Force: true})
	})
	require.NoError(t, mustStart(ctx, cli, ctr))

	f, err := os.Open(bin)
	require.NoError(t, err)
	fi, _ := f.Stat()
	require.NoError(t, dockerx.CopyFileFromHost(ctx, cli, ctr, "/usr/local/bin", "claude", 0o755, f, fi.Size()))
	f.Close()

	out, code, err := dockerx.ExecOutput(ctx, cli, ctr, "", []string{"claude", "--version"})
	require.NoError(t, err)
	require.Equal(t, 0, code, "claude --version failed: %s", out)
	t.Logf("claude --version -> %s", strings.TrimSpace(out))
	require.Contains(t, out, version)
}

func mustStart(ctx context.Context, cli *client.Client, ctr string) error {
	_, err := cli.ContainerStart(ctx, ctr, client.ContainerStartOptions{})
	return err
}

func drain(r interface{ Close() error }) {
	if rc, ok := r.(interface{ Read([]byte) (int, error) }); ok {
		buf := make([]byte, 4096)
		for {
			if _, err := rc.Read(buf); err != nil {
				break
			}
		}
	}
	r.Close()
}
