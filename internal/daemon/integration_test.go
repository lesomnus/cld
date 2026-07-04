package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/lesomnus/cld/internal/release"
	"github.com/lesomnus/cld/internal/tmuxx"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

const test_image = "alpine:3.20"

// fake_claude pretends to be the claude binary inside the container.
const fake_claude = `#!/bin/sh
echo "claude started in $PWD as $(id -un) args:$*"
echo "config: $CLAUDE_CONFIG_DIR"
sleep 2147483647
`

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

// build_cld builds the real cld binary; it is copied into containers as the
// watcher and run by tmux panes, so the test binary itself must not be used.
func build_cld(t *testing.T) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), "cld")
	cmd := exec.Command("go", "build", "-o", out, "github.com/lesomnus/cld")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build cld: %v: %s", err, data)
	}
	return out
}

// fake_release serves a release channel with one version for every platform.
func fake_release(t *testing.T, version string, binary []byte) *httptest.Server {
	t.Helper()

	sum := sha256.Sum256(binary)
	manifest := release.Manifest{
		Version:   version,
		Platforms: map[release.Platform]release.ManifestEntry{},
	}
	for _, p := range []release.Platform{"linux-x64", "linux-arm64", "linux-x64-musl", "linux-arm64-musl"} {
		manifest.Platforms[p] = release.ManifestEntry{
			Binary:   "claude",
			Checksum: hex.EncodeToString(sum[:]),
			Size:     int64(len(binary)),
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/stable", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, version)
	})
	mux.HandleFunc("/"+version+"/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(manifest)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/claude") {
			w.Write(binary)
			return
		}
		http.NotFound(w, r)
	})

	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func pull_image(t *testing.T, cli *client.Client) {
	t.Helper()
	pull_ref(t, cli, test_image)
}

func pull_ref(t *testing.T, cli *client.Client, ref string) {
	t.Helper()

	res, err := cli.ImagePull(t.Context(), ref, client.ImagePullOptions{})
	require.NoError(t, err)
	defer res.Close()
	io.Copy(io.Discard, res)
}

// run_devcontainer creates and starts a fake devcontainer on the engine, run as
// root.
func run_devcontainer(t *testing.T, cli *client.Client, local_folder string) string {
	return run_devcontainer_as(t, cli, local_folder, test_image, "root")
}

// run_devcontainer_as is run_devcontainer with a chosen image and remoteUser,
// so a test can exercise a non-root container user — the realistic devcontainer
// case, and the only one that surfaces config-tree ownership bugs.
func run_devcontainer_as(t *testing.T, cli *client.Client, local_folder, image, remoteUser string) string {
	t.Helper()

	created, err := cli.ContainerCreate(t.Context(), client.ContainerCreateOptions{
		Config: &container.Config{
			Image: image,
			Cmd:   []string{"sleep", "2147483647"},
			Labels: map[string]string{
				devc.LabelLocalFolder: local_folder,
				devc.LabelConfigFile:  filepath.Join(local_folder, ".devcontainer", "devcontainer.json"),
				devc.LabelMetadata:    fmt.Sprintf(`[{"remoteUser":%q}]`, remoteUser),
			},
		},
		HostConfig: &container.HostConfig{
			// The bind source lives inside the DinD engine; docker creates it.
			Binds: []string{local_folder + ":/workspace"},
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

// run_container_labeled creates and starts a container with the given labels,
// binding local_folder to /workspace when it is non-empty. It stands up
// containers cld should leave alone: a devcontainer marked cld.ignore, or a
// plain container that carries no devcontainer label at all.
func run_container_labeled(t *testing.T, cli *client.Client, local_folder string, labels map[string]string) string {
	t.Helper()

	host := &container.HostConfig{}
	if local_folder != "" {
		host.Binds = []string{local_folder + ":/workspace"}
	}
	created, err := cli.ContainerCreate(t.Context(), client.ContainerCreateOptions{
		Config: &container.Config{
			Image:  test_image,
			Cmd:    []string{"sleep", "2147483647"},
			Labels: labels,
		},
		HostConfig: host,
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

func wait_for(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func find_item(items []Item, name string) *Item {
	for _, it := range items {
		if it.Name == name {
			return &it
		}
	}
	return nil
}

func TestDaemon(t *testing.T) {
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

	tmux := &tmuxx.Server{Socket: cfg.TmuxSocketPath()}
	t.Cleanup(func() {
		exec.Command("tmux", "-S", cfg.TmuxSocketPath(), "kill-server").Run()
	})

	wait_for(t, 10*time.Second, "daemon socket", func() bool {
		_, err := FetchItems(context.Background(), cfg.SocketPath())
		return err == nil
	})

	local_folder := fmt.Sprintf("/tmp/proj-%d", time.Now().UnixNano())
	name := devc.DisplayName(local_folder)
	ctr := run_devcontainer(t, cli, local_folder)

	t.Run("provisions a new devcontainer", func(t *testing.T) {
		wait_for(t, 60*time.Second, "item ready", func() bool {
			items, err := FetchItems(context.Background(), cfg.SocketPath())
			if err != nil {
				return false
			}
			it := find_item(items, name)
			return it != nil && it.Status == StatusReady
		})

		items, err := FetchItems(context.Background(), cfg.SocketPath())
		require.NoError(t, err)
		it := find_item(items, name)
		require.NotNil(t, it)
		require.Equal(t, "/workspace", it.Workspace)
		require.Equal(t, "9.9.9", it.Version)
	})
	t.Run("installs the binaries", func(t *testing.T) {
		out, code, err := dockerx.ExecOutput(t.Context(), cli, ctr, "", []string{"readlink", "/usr/local/bin/claude"})
		require.NoError(t, err)
		require.Equal(t, 0, code)
		require.Equal(t, "claude-9.9.9", strings.TrimSpace(out))

		ok, err := dockerx.PathExists(t.Context(), cli, ctr, "/usr/local/bin/cld")
		require.NoError(t, err)
		require.True(t, ok)
	})
	t.Run("seeds onboarding and retention state", func(t *testing.T) {
		out, code, err := dockerx.ExecOutput(t.Context(), cli, ctr, "", []string{"cat", "/root/.cld/claude/.claude.json"})
		require.NoError(t, err)
		require.Equal(t, 0, code)

		var state map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &state))
		require.Equal(t, true, state["hasCompletedOnboarding"])
		project := state["projects"].(map[string]any)["/workspace"].(map[string]any)
		require.Equal(t, true, project["hasTrustDialogAccepted"])

		out, _, err = dockerx.ExecOutput(t.Context(), cli, ctr, "", []string{"cat", "/root/.cld/claude/settings.json"})
		require.NoError(t, err)
		require.Contains(t, out, "cleanupPeriodDays")
	})
	t.Run("runs claude in a host tmux session", func(t *testing.T) {
		has, err := tmux.HasSession(t.Context(), devc.SessionName(name))
		require.NoError(t, err)
		require.True(t, has)

		capture := func() string {
			out, _ := exec.Command(
				"tmux", "-S", cfg.TmuxSocketPath(),
				// "=name" is a session target; capture-pane wants a pane target.
				"capture-pane", "-p", "-t", devc.SessionName(name)+":0",
			).Output()
			return string(out)
		}
		wait_for(t, 30*time.Second, "claude output in pane", func() bool {
			return strings.Contains(capture(), "claude started in /workspace as root")
		})
		// The session env must actually reach the pane (regression: a
		// mode-gated flag handler silently dropped every --env).
		require.Contains(t, capture(), "config: /root/.cld/claude")
	})
	t.Run("watcher syncs changes out to the backup", func(t *testing.T) {
		_, code, err := dockerx.ExecOutput(t.Context(), cli, ctr, "", []string{
			"sh", "-c",
			`mkdir -p /root/.cld/claude/projects/-workspace` +
				` && echo '{"cwd":"/workspace"}' > /root/.cld/claude/projects/-workspace/s1.jsonl` +
				` && echo secret > /root/.cld/claude/.credentials.json`,
		})
		require.NoError(t, err)
		require.Equal(t, 0, code)

		var project_backup string
		wait_for(t, 20*time.Second, "backup files", func() bool {
			matches, _ := filepath.Glob(filepath.Join(cfg.DataDir, "projects", "*", "projects", "-workspace", "s1.jsonl"))
			if len(matches) == 0 {
				return false
			}
			project_backup = matches[0]

			_, err := os.Stat(filepath.Join(cfg.GlobalBackupDir(), ".credentials.json"))
			return err == nil
		})

		data, err := os.ReadFile(project_backup)
		require.NoError(t, err)
		require.Contains(t, string(data), `"cwd":"/workspace"`)
	})
	t.Run("restores the backup into a recreated container", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := cli.ContainerRemove(ctx, ctr, client.ContainerRemoveOptions{Force: true})
		require.NoError(t, err)

		wait_for(t, 20*time.Second, "item retired", func() bool {
			items, err := FetchItems(context.Background(), cfg.SocketPath())
			return err == nil && find_item(items, name) == nil
		})

		ctr2 := run_devcontainer(t, cli, local_folder)
		wait_for(t, 60*time.Second, "recreated item ready", func() bool {
			items, err := FetchItems(context.Background(), cfg.SocketPath())
			if err != nil {
				return false
			}
			it := find_item(items, name)
			return it != nil && it.Status == StatusReady
		})

		out, code, err := dockerx.ExecOutput(t.Context(), cli, ctr2, "", []string{"cat", "/root/.cld/claude/projects/-workspace/s1.jsonl"})
		require.NoError(t, err)
		require.Equal(t, 0, code)
		require.Contains(t, out, `"cwd":"/workspace"`)

		out, code, err = dockerx.ExecOutput(t.Context(), cli, ctr2, "", []string{"cat", "/root/.cld/claude/.credentials.json"})
		require.NoError(t, err)
		require.Equal(t, 0, code)
		require.Contains(t, out, "secret")

		// History exists now, so the new session resumes the conversation.
		wait_for(t, 30*time.Second, "claude resumed in pane", func() bool {
			out, err := exec.Command(
				"tmux", "-S", cfg.TmuxSocketPath(),
				"capture-pane", "-p", "-t", devc.SessionName(name)+":0",
			).Output()
			return err == nil && strings.Contains(string(out), "args:--continue")
		})
	})
}

// TestLegacyCredentialBootstrap: a container whose default ~/.claude carries
// credentials (a user's bind mount) gets them copied into cld's own config
// dir — but only when no backup supplied them (backup wins; hence the
// isolated daemon with an empty DataDir).
func TestLegacyCredentialBootstrap(t *testing.T) {
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
	t.Cleanup(func() { exec.Command("tmux", "-S", cfg.TmuxSocketPath(), "kill-server").Run() })
	_, stop := start_daemon(t, cfg, cli, build_cld(t))
	defer stop()

	lf := fmt.Sprintf("/tmp/legacy-%d", time.Now().UnixNano())
	created, err := cli.ContainerCreate(t.Context(), client.ContainerCreateOptions{
		Config: &container.Config{
			Image: test_image,
			Cmd: []string{"sh", "-c",
				`mkdir -p /root/.claude && echo legacy-cred > /root/.claude/.credentials.json && sleep 2147483647`},
			Labels: map[string]string{
				devc.LabelLocalFolder: lf,
				devc.LabelMetadata:    `[{"remoteUser":"root"}]`,
			},
		},
		HostConfig: &container.HostConfig{Binds: []string{lf + ":/workspace"}},
	})
	require.NoError(t, err)
	legacy_ctr := created.ID
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cli.ContainerRemove(c, legacy_ctr, client.ContainerRemoveOptions{Force: true})
	})
	_, err = cli.ContainerStart(t.Context(), legacy_ctr, client.ContainerStartOptions{})
	require.NoError(t, err)

	wait_for(t, 60*time.Second, "legacy container ready", func() bool {
		it := find_item(must_items(t, cfg), devc.DisplayName(lf))
		return it != nil && it.Status == StatusReady
	})
	out, code, err := dockerx.ExecOutput(t.Context(), cli, legacy_ctr, "", []string{
		"cat", "/root/.cld/claude/.credentials.json",
	})
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Contains(t, out, "legacy-cred")
}

// start_daemon builds a daemon on cfg and runs it until the returned stop is
// called (which also drains workers).
func start_daemon(t *testing.T, cfg *config.Config, cli *client.Client, self string) (*Daemon, func()) {
	t.Helper()

	d, err := New(cfg, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)
	d.self = self

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()

	wait_for(t, 10*time.Second, "daemon socket", func() bool {
		_, err := FetchItems(context.Background(), cfg.SocketPath())
		return err == nil
	})
	return d, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("daemon did not shut down")
		}
	}
}

func TestSessionLifecycle(t *testing.T) {
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
	self := build_cld(t)
	t.Cleanup(func() { exec.Command("tmux", "-S", cfg.TmuxSocketPath(), "kill-server").Run() })

	_, stop := start_daemon(t, cfg, cli, self)
	started := true
	defer func() {
		if started {
			stop()
		}
	}()

	local_folder := fmt.Sprintf("/tmp/proj-%d", time.Now().UnixNano())
	name := devc.DisplayName(local_folder)
	ctr := run_devcontainer(t, cli, local_folder)

	wait_for(t, 60*time.Second, "ready", func() bool {
		it := find_item(must_items(t, cfg), name)
		return it != nil && it.Status == StatusReady
	})
	id := find_item(must_items(t, cfg), name).ID

	endSession := func(t *testing.T) {
		t.Helper()
		// Empty gen means "current generation" and is always accepted; code 0
		// is a clean quit (the user ending the session).
		require.NoError(t, NotifyExited(context.Background(), cfg.SocketPath(), id, "", 0))
		wait_for(t, 10*time.Second, "session-ended", func() bool {
			it := find_item(must_items(t, cfg), name)
			return it != nil && it.Status == StatusSessionEnded
		})
	}

	t.Run("the daemon API is reachable from inside the container via the relay", func(t *testing.T) {
		// An in-container `cld` dials the relayed socket at $HOME/.cache/cld and
		// reaches the daemon's own API, so `cld ls` there lists this container.
		var out string
		wait_for(t, 20*time.Second, "in-container cld via api relay", func() bool {
			o, code, err := dockerx.ExecOutput(t.Context(), cli, ctr, "0",
				[]string{"sh", "-c", "HOME=/root /usr/local/bin/cld ls"})
			if err != nil || code != 0 {
				return false
			}
			out = o
			return strings.Contains(o, name)
		})
		require.Contains(t, out, name)
	})

	t.Run("seeds a vscode terminal profile into the container", func(t *testing.T) {
		var out string
		wait_for(t, 15*time.Second, "vscode profile seeded", func() bool {
			o, code, err := dockerx.ExecOutput(t.Context(), cli, ctr, "0",
				[]string{"cat", "/root/.vscode-server/data/Machine/settings.json"})
			if err != nil || code != 0 {
				return false
			}
			out = o
			return strings.Contains(o, `"claude"`)
		})
		require.Contains(t, out, "terminal.integrated.profiles.linux")
		require.Contains(t, out, `"cld"`)
	})

	t.Run("a non-zero session exit is surfaced as failed", func(t *testing.T) {
		require.NoError(t, NotifyExited(context.Background(), cfg.SocketPath(), id, "", 1))
		wait_for(t, 10*time.Second, "failed", func() bool {
			it := find_item(must_items(t, cfg), name)
			return it != nil && it.Status == StatusFailed
		})
		require.Contains(t, find_item(must_items(t, cfg), name).Error, "status 1")

		// `cld it --new` recovers a failed session back to a live one.
		require.NoError(t, RecreateSession(context.Background(), cfg.SocketPath(), name))
		wait_for(t, 10*time.Second, "ready again", func() bool {
			it := find_item(must_items(t, cfg), name)
			return it != nil && it.Status == StatusReady
		})
	})

	t.Run("ending the session marks it session-ended", endSession)

	t.Run("a container restart of an ended container returns to ready", func(t *testing.T) {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel2()
		_, err := cli.ContainerRestart(ctx2, ctr, client.ContainerRestartOptions{})
		require.NoError(t, err)

		// New generation → a fresh session, and the status must not stay
		// stuck at session-ended.
		wait_for(t, 30*time.Second, "ready after container restart", func() bool {
			it := find_item(must_items(t, cfg), name)
			return it != nil && it.Status == StatusReady
		})
		tmux := &tmuxx.Server{Socket: cfg.TmuxSocketPath()}
		has, err := tmux.HasSession(context.Background(), devc.SessionName(name))
		require.NoError(t, err)
		require.True(t, has)

		// Re-end so the next subtest exercises the daemon-restart path.
		endSession(t)
	})

	t.Run("a daemon restart does not resurrect the ended session", func(t *testing.T) {
		stop()
		started = false

		exec.Command("tmux", "-S", cfg.TmuxSocketPath(), "kill-server").Run()

		_, stop2 := start_daemon(t, cfg, cli, self)
		defer stop2()

		wait_for(t, 30*time.Second, "item seen after restart", func() bool {
			return find_item(must_items(t, cfg), name) != nil
		})
		// Give reconcile a moment; the status must stay session-ended and no
		// tmux session must be recreated.
		time.Sleep(2 * time.Second)
		it := find_item(must_items(t, cfg), name)
		require.NotNil(t, it)
		require.Equal(t, StatusSessionEnded, it.Status)

		tmux := &tmuxx.Server{Socket: cfg.TmuxSocketPath()}
		has, err := tmux.HasSession(context.Background(), devc.SessionName(name))
		require.NoError(t, err)
		require.False(t, has, "ended session must not be resurrected")

		t.Run("it --new recreates the session", func(t *testing.T) {
			require.NoError(t, RecreateSession(context.Background(), cfg.SocketPath(), name))
			has, err := tmux.HasSession(context.Background(), devc.SessionName(name))
			require.NoError(t, err)
			require.True(t, has)

			it := find_item(must_items(t, cfg), name)
			require.Equal(t, StatusReady, it.Status)
		})
	})

	_ = ctr
}

func TestNameKeying(t *testing.T) {
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
	self := build_cld(t)
	t.Cleanup(func() { exec.Command("tmux", "-S", cfg.TmuxSocketPath(), "kill-server").Run() })

	_, stop := start_daemon(t, cfg, cli, self)
	defer stop()

	// The daemon reads devcontainer.json from the config_file label path on its
	// own filesystem, so write one carrying a name.
	lf := fmt.Sprintf("/tmp/named-%d", time.Now().UnixNano())
	require.NoError(t, os.MkdirAll(filepath.Join(lf, ".devcontainer"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(lf, ".devcontainer", "devcontainer.json"),
		[]byte(`{"name": "acme/api", "workspaceFolder": "/workspace"}`), 0o644))

	ctr := run_devcontainer(t, cli, lf)
	_ = ctr

	wait_for(t, 60*time.Second, "ready", func() bool {
		it := find_item(must_items(t, cfg), "acme-api")
		return it != nil && it.Status == StatusReady
	})

	t.Run("display name is the slugged devcontainer name", func(t *testing.T) {
		require.NotNil(t, find_item(must_items(t, cfg), "acme-api"))
	})
	t.Run("backup is keyed by the name, not the path", func(t *testing.T) {
		// A sync must land under projects/cld-acme-api/, not a path hash.
		_, _, err := dockerx.ExecOutput(t.Context(), cli, ctr, "", []string{
			"sh", "-c",
			`mkdir -p /root/.cld/claude/projects/-workspace` +
				` && echo '{"cwd":"/workspace"}' > /root/.cld/claude/projects/-workspace/s1.jsonl`,
		})
		require.NoError(t, err)

		want := filepath.Join(cfg.ProjectBackupDir("cld-acme-api"), "projects", "-workspace", "s1.jsonl")
		wait_for(t, 20*time.Second, "name-keyed backup", func() bool {
			_, err := os.Stat(want)
			return err == nil
		})
	})
}

func must_items(t *testing.T, cfg *config.Config) []Item {
	t.Helper()
	items, err := FetchItems(context.Background(), cfg.SocketPath())
	if err != nil {
		return nil
	}
	return items
}
