package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

const runner_dockerfile = `FROM node:22-alpine
RUN apk add --no-cache docker-cli docker-cli-buildx docker-cli-compose git \
	&& npm install -g @devcontainers/cli
ENTRYPOINT ["devcontainer"]
`

// TestUpE2E drives the full `cld up` loop with the real devcontainer CLI:
// it builds the runner image (real npm install), runs `cld up` from a
// container that has nothing but the cld binary and Docker access, and
// asserts that the daemon provisions the devcontainer the CLI created.
// Gated behind CLD_E2E_REAL: hits npm and Docker Hub.
func TestUpE2E(t *testing.T) {
	if os.Getenv("CLD_E2E_REAL") == "" {
		t.Skip("set CLD_E2E_REAL=1 to run the real devcontainer-cli E2E test")
	}
	cli := require_docker(t)
	pull_image(t, cli)

	build_runner_image(t, cli, "cld-runner:test")

	server := fake_release(t, "9.9.9", []byte(fake_claude))
	tmp := t.TempDir()
	cfg := &config.Config{
		CacheDir: filepath.Join(tmp, "cache"),
		DataDir:  filepath.Join(tmp, "data"),
		Release:  config.ReleaseConfig{BaseURL: server.URL, Channel: "stable", CheckInterval: config.Duration(time.Hour)},
		Sync:     config.SyncConfig{Debounce: config.Duration(200 * time.Millisecond), FallbackInterval: config.Duration(time.Minute)},
	}
	d, err := New(cfg, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)
	d.self = build_cld(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go d.Run(ctx)
	t.Cleanup(func() { exec.Command("tmux", "-S", cfg.TmuxSocketPath(), "kill-server").Run() })

	// The workspace lives on the engine host; a helper container writes the
	// fixture through a bind mount.
	ws := fmt.Sprintf("/tmp/upfix-%d", time.Now().UnixNano())
	prep := run_helper(t, cli, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: test_image,
			Cmd: []string{"sh", "-c",
				`mkdir -p /ws/.devcontainer && printf '%s' '{"name":"upx","image":"alpine:3.20"}' > /ws/.devcontainer/devcontainer.json`},
		},
		HostConfig: &container.HostConfig{Binds: []string{ws + ":/ws"}},
	})
	require.Equal(t, int64(0), prep)

	// The client has only the cld binary, the engine socket, and the
	// workspace mounted at its host path — the shape of a bare host.
	created, err := cli.ContainerCreate(t.Context(), client.ContainerCreateOptions{
		Config: &container.Config{
			Image: test_image,
			Cmd:   []string{"sleep", "600"},
			Env:   []string{"DOCKER_HOST=unix:///var/run/docker.sock"},
		},
		HostConfig: &container.HostConfig{Binds: []string{
			"/var/run/docker.sock:/var/run/docker.sock",
			ws + ":" + ws,
		}},
	})
	require.NoError(t, err)
	ctr := created.ID
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cli.ContainerRemove(c, ctr, client.ContainerRemoveOptions{Force: true})
	})
	_, err = cli.ContainerStart(t.Context(), ctr, client.ContainerStartOptions{})
	require.NoError(t, err)

	self, err := os.Open(d.self)
	require.NoError(t, err)
	fi, _ := self.Stat()
	require.NoError(t, dockerx.CopyFileFromHost(t.Context(), cli, ctr, "/usr/local/bin", "cld", 0o755, self, fi.Size()))
	self.Close()
	require.NoError(t, dockerx.WriteFile(t.Context(), cli, ctr, "/", "cld.e2e.yaml", 0o644, 0, 0,
		[]byte("up:\n  image: cld-runner:test\n")))

	out, code, err := dockerx.ExecOutput(t.Context(), cli, ctr, "", []string{
		"cld", "--config", "/cld.e2e.yaml", "up", "--no-attach", ws,
	})
	require.NoError(t, err)
	require.Equal(t, 0, code, "cld up failed:\n%s", out)
	t.Logf("cld up output:\n%s", out)

	// The daemon must pick the new devcontainer up and provision it, keyed by
	// the workspace path `cld up` passed to the CLI.
	wait_for(t, 120*time.Second, "provisioned by the daemon", func() bool {
		for _, it := range must_items(t, cfg) {
			if it.LocalFolder == ws && it.Status == StatusReady {
				return true
			}
		}
		return false
	})
}

// build_runner_image builds the devcontainer-cli runner on the engine.
func build_runner_image(t *testing.T, cli *client.Client, tag string) {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "Dockerfile", Mode: 0o644, Size: int64(len(runner_dockerfile)),
	}))
	_, err := tw.Write([]byte(runner_dockerfile))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	res, err := cli.ImageBuild(t.Context(), &buf, client.ImageBuildOptions{
		Tags:   []string{tag},
		Remove: true,
	})
	require.NoError(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	require.NotContains(t, string(body), `"error"`, "image build failed:\n%s", body)
}

// run_helper runs a one-shot container to completion and returns its exit code.
func run_helper(t *testing.T, cli *client.Client, opts client.ContainerCreateOptions) int64 {
	t.Helper()

	created, err := cli.ContainerCreate(t.Context(), opts)
	require.NoError(t, err)
	id := created.ID
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cli.ContainerRemove(c, id, client.ContainerRemoveOptions{Force: true})
	})

	att, err := cli.ContainerAttach(t.Context(), id, client.ContainerAttachOptions{
		Stream: true, Stdout: true, Stderr: true,
	})
	require.NoError(t, err)
	defer att.Close()

	_, err = cli.ContainerStart(t.Context(), id, client.ContainerStartOptions{})
	require.NoError(t, err)

	var out strings.Builder
	stdcopy.StdCopy(&out, &out, att.Reader)

	wait := cli.ContainerWait(t.Context(), id, client.ContainerWaitOptions{})
	select {
	case err := <-wait.Error:
		t.Fatalf("wait helper: %v", err)
		return -1
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			t.Logf("helper output:\n%s", out.String())
		}
		return res.StatusCode
	}
}
