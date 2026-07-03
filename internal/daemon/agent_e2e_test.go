package daemon

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crypto/ed25519"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh/agent"
)

// TestAgentRelayE2E runs a real ssh-agent on the "host" side (the test
// process) and verifies that a provisioned container, whose SSH_AUTH_SOCK
// points at cld's relay, sees the very key that agent holds — proving the
// relay carries the ssh-agent protocol end to end.
func TestAgentRelayE2E(t *testing.T) {
	cli := require_docker(t)
	pull_image(t, cli)
	server := fake_release(t, "9.9.9", []byte(fake_claude))

	tmp := t.TempDir()
	cfg := &config.Config{
		CacheDir: filepath.Join(tmp, "cache"),
		DataDir:  filepath.Join(tmp, "data"),
		Release:  config.ReleaseConfig{BaseURL: server.URL, Channel: "stable", CheckInterval: config.Duration(time.Hour)},
		Sync:     config.SyncConfig{Debounce: config.Duration(200 * time.Millisecond), FallbackInterval: config.Duration(time.Minute)},
	}
	require.NoError(t, os.MkdirAll(cfg.CacheDir, 0o755))

	// Stage a gitconfig before provisioning, as `cld it`/`up` would, so the
	// daemon installs it into the session (the timing the review flagged).
	require.NoError(t, os.WriteFile(cfg.GitConfigPath(),
		[]byte("[user]\n\tname = E2E Tester\n"), 0o644))

	// A real in-process ssh-agent holding one key, served on the exact path
	// the daemon resolves as its agent source (cache/agent.sock).
	comment := "e2e-relay-key"
	serveTestAgent(t, cfg.AgentSocketPath(), comment)

	d, err := New(cfg, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)
	d.self = build_cld(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go d.Run(ctx)
	t.Cleanup(func() { exec.Command("tmux", "-S", cfg.TmuxSocketPath(), "kill-server").Run() })
	wait_for(t, 10*time.Second, "daemon socket", func() bool {
		_, e := FetchItems(context.Background(), cfg.SocketPath())
		return e == nil
	})

	lf := "/tmp/agent-" + strings.ReplaceAll(t.Name(), "/", "-")
	name := devc.DisplayName(lf)
	ctr := run_devcontainer(t, cli, lf)

	wait_for(t, 60*time.Second, "ready", func() bool {
		it := find_item(must_items(t, cfg), name)
		return it != nil && it.Status == StatusReady
	})

	// Install an ssh client and, with SSH_AUTH_SOCK at cld's relay socket,
	// list the agent's identities from inside the container.
	_, code, err := dockerx.ExecOutput(t.Context(), cli, ctr, "root", []string{
		"sh", "-c", "apk add --no-cache openssh-client >/dev/null 2>&1",
	})
	require.NoError(t, err)
	require.Equal(t, 0, code)

	var out string
	wait_for(t, 20*time.Second, "agent visible in container", func() bool {
		out, code, err = dockerx.ExecOutput(t.Context(), cli, ctr, "root", []string{
			"sh", "-c", "SSH_AUTH_SOCK=/root/.cld/claude/agent.sock ssh-add -l",
		})
		return err == nil && code == 0 && strings.Contains(out, comment)
	})
	require.Contains(t, out, comment, "the host key must be visible through the relay")

	t.Run("host gitconfig is installed into the session dir", func(t *testing.T) {
		out, code, err := dockerx.ExecOutput(t.Context(), cli, ctr, "root", []string{
			"cat", "/root/.cld/claude/gitconfig",
		})
		require.NoError(t, err)
		require.Equal(t, 0, code)
		require.Contains(t, out, "E2E Tester")
	})
}

// serveTestAgent runs an ssh agent with one generated key on a unix socket.
func serveTestAgent(t *testing.T, sock string, comment string) {
	t.Helper()

	keyring := agent.NewKeyring()
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NoError(t, keyring.Add(agent.AddedKey{PrivateKey: priv, Comment: comment}))

	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go agent.ServeAgent(keyring, c)
		}
	}()
	// Sanity: the agent answers.
	c, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer c.Close()
	_, err = agent.NewClient(c).List()
	require.NoError(t, err)
}
