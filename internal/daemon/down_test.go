package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// TestDownAll verifies `cld down --all` removes every devcontainer cld manages
// while leaving alone containers it does not: a devcontainer opted out with
// cld.ignore and a plain (non-devcontainer) container both survive and are
// never even tracked. The final backup still runs for what is removed.
func TestDownAll(t *testing.T) {
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

	// Two managed devcontainers cld should remove.
	folderA := fmt.Sprintf("/tmp/proj-downall-a-%d", time.Now().UnixNano())
	folderB := fmt.Sprintf("/tmp/proj-downall-b-%d", time.Now().UnixNano())
	nameA := devc.DisplayName(folderA)
	nameB := devc.DisplayName(folderB)
	ctrA := run_devcontainer(t, cli, folderA)
	ctrB := run_devcontainer(t, cli, folderB)

	// A devcontainer explicitly opted out with cld.ignore, and a plain container
	// that is not a devcontainer at all: neither must be tracked or removed.
	folderIgnored := fmt.Sprintf("/tmp/proj-downall-ign-%d", time.Now().UnixNano())
	ctrIgnored := run_container_labeled(t, cli, folderIgnored, map[string]string{
		devc.LabelLocalFolder: folderIgnored,
		devc.LabelConfigFile:  filepath.Join(folderIgnored, ".devcontainer", "devcontainer.json"),
		devc.LabelMetadata:    `[{"remoteUser":"root"}]`,
		devc.LabelIgnore:      "true",
	})
	ctrPlain := run_container_labeled(t, cli, "", map[string]string{
		"com.example.role": "sidecar",
	})

	// Both managed containers reach ready.
	wait_for(t, 90*time.Second, "both items ready", func() bool {
		items, err := FetchItems(context.Background(), cfg.SocketPath())
		if err != nil {
			return false
		}
		a := find_item(items, nameA)
		b := find_item(items, nameB)
		return a != nil && a.Status == StatusReady && b != nil && b.Status == StatusReady
	})

	// The ignored devcontainer is never tracked; only the two managed ones are.
	items, err := FetchItems(context.Background(), cfg.SocketPath())
	require.NoError(t, err)
	require.Nil(t, find_item(items, devc.DisplayName(folderIgnored)), "ignored devcontainer must not be tracked")
	require.Len(t, items, 2, "only the two managed devcontainers are tracked")

	// Seed a conversation in A so there is something to back up on the way down.
	_, code, err := dockerx.ExecOutput(t.Context(), cli, ctrA, "", []string{
		"sh", "-c",
		`mkdir -p /root/.cld/claude/projects/-workspace` +
			` && echo '{"cwd":"/workspace"}' > /root/.cld/claude/projects/-workspace/s1.jsonl`,
	})
	require.NoError(t, err)
	require.Equal(t, 0, code)

	// Remove everything cld manages.
	results, err := DownAll(context.Background(), cfg.SocketPath())
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, r := range results {
		require.True(t, r.OK, "down %s: %s", r.Name, r.Error)
	}

	// The listing is now empty.
	wait_for(t, 20*time.Second, "items gone", func() bool {
		items, err := FetchItems(context.Background(), cfg.SocketPath())
		return err == nil && len(items) == 0
	})

	// The two managed containers are gone from the engine.
	_, err = cli.ContainerInspect(context.Background(), ctrA, client.ContainerInspectOptions{})
	require.Error(t, err, "managed container A should be removed by down --all")
	_, err = cli.ContainerInspect(context.Background(), ctrB, client.ContainerInspectOptions{})
	require.Error(t, err, "managed container B should be removed by down --all")

	// The ignored and plain containers survive untouched.
	_, err = cli.ContainerInspect(context.Background(), ctrIgnored, client.ContainerInspectOptions{})
	require.NoError(t, err, "cld.ignore container must survive down --all")
	_, err = cli.ContainerInspect(context.Background(), ctrPlain, client.ContainerInspectOptions{})
	require.NoError(t, err, "plain non-devcontainer must survive down --all")

	// A final backup ran for A before removal, so its conversation survived.
	matches, _ := filepath.Glob(filepath.Join(cfg.DataDir, "projects", "*", "projects", "-workspace", "s1.jsonl"))
	require.NotEmpty(t, matches, "conversation backup should be kept after down --all")
}

// TestDownAllSkipsUnmanaged pins the scope guarantee against the transient
// window the steady-state TestDownAll cannot reach: an entry is tracked for
// every started container before ensure classifies it (and a non-running one is
// never classified), so at down --all time a container may be tracked yet not be
// a cld-managed devcontainer. This drives handle_down_all directly with two such
// entries already in the map — a cld.ignore devcontainer and a plain
// non-devcontainer, both made to look fully provisioned — and asserts the live
// re-inspect leaves them untouched and unreported rather than trusting tracking.
func TestDownAllSkipsUnmanaged(t *testing.T) {
	cli := require_docker(t)
	pull_image(t, cli)

	tmp := t.TempDir()
	cfg := &config.Config{
		CacheDir: filepath.Join(tmp, "cache"),
		DataDir:  filepath.Join(tmp, "data"),
	}
	d, err := New(cfg, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)
	// handle_down_all posts the removal decision under d.base_ctx; set it since
	// the event loop (which normally sets it in Run) is deliberately not started.
	d.base_ctx = t.Context()

	// Containers cld must never remove: one opted out with cld.ignore, one that
	// is not a devcontainer at all.
	ignoredFolder := fmt.Sprintf("/tmp/proj-skip-ign-%d", time.Now().UnixNano())
	ctrIgnored := run_container_labeled(t, cli, ignoredFolder, map[string]string{
		devc.LabelLocalFolder: ignoredFolder,
		devc.LabelConfigFile:  filepath.Join(ignoredFolder, ".devcontainer", "devcontainer.json"),
		devc.LabelMetadata:    `[{"remoteUser":"root"}]`,
		devc.LabelIgnore:      "true",
	})
	ctrPlain := run_container_labeled(t, cli, "", map[string]string{
		"com.example.role": "db",
	})

	// Track both as reconcile/a start event would, and give them a name and
	// workspace so the entry looks fully provisioned — the naive "trust the
	// tracked set" path would remove them; the live re-inspect must not.
	for _, id := range []string{ctrIgnored, ctrPlain} {
		e := d.get_or_create(id)
		e.item.Name = "would-be-" + short(id)
		e.item.Workspace = "/workspace"
		e.publish()
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://cld/down/all", nil)
	d.handle_down_all(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Results []DownResult `json:"results"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.Empty(t, body.Results, "no unmanaged container may be reported removed")

	// Both are still alive on the engine.
	_, err = cli.ContainerInspect(t.Context(), ctrIgnored, client.ContainerInspectOptions{})
	require.NoError(t, err, "cld.ignore container must survive down --all even while tracked")
	_, err = cli.ContainerInspect(t.Context(), ctrPlain, client.ContainerInspectOptions{})
	require.NoError(t, err, "plain non-devcontainer must survive down --all even while tracked")
}

// TestDownTargetsSparesIgnoredSibling verifies that tearing down a Compose
// devcontainer sweeps its project — but not a sibling the user explicitly
// marked cld.ignore, so the opt-out is honored even inside a managed project. A
// normal (unlabelled) sidecar is still swept, as it is part of the project.
func TestDownTargetsSparesIgnoredSibling(t *testing.T) {
	cli := require_docker(t)
	pull_image(t, cli)

	d, err := New(&config.Config{CacheDir: t.TempDir()}, cli, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	require.NoError(t, err)

	project := fmt.Sprintf("cld-test-proj-%d", time.Now().UnixNano())
	// The managed devcontainer (main service of the compose project).
	main := run_container_labeled(t, cli, "", map[string]string{
		composeProjectLabel:   project,
		devc.LabelLocalFolder: "/tmp/some-proj",
	})
	// A normal sidecar (e.g. a db) — part of the project, should be swept.
	sidecar := run_container_labeled(t, cli, "", map[string]string{
		composeProjectLabel: project,
	})
	// A sidecar the user explicitly opted out of — must be spared.
	ignored := run_container_labeled(t, cli, "", map[string]string{
		composeProjectLabel: project,
		devc.LabelIgnore:    "true",
	})

	containers, _ := d.down_targets(t.Context(), main)
	require.Contains(t, containers, main, "the devcontainer itself is always a target")
	require.Contains(t, containers, sidecar, "a normal sidecar is part of the project sweep")
	require.NotContains(t, containers, ignored, "a cld.ignore sibling must be spared from the sweep")
}
