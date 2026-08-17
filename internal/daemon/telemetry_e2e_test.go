package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/stretchr/testify/require"
)

// TestTelemetryRelayE2E drives the whole collection chain against a real
// container: the daemon points the session at the loopback endpoint
// (session_env), relay_otlp serves the receiver over a docker exec, `cld x
// otlp` listens on that port inside the container, and an OTLP/JSON body posted
// from IN the container has to surface as this container's totals in the
// listing.
//
// It stands in for claude's own exporter, which cannot be driven from a test —
// the fake claude binary the e2e harness installs emits no telemetry. What it
// proves is the part cld owns: that the port claude is told to use is reachable,
// speaks OTLP/JSON, and attributes what it receives to the right container.
func TestTelemetryRelayE2E(t *testing.T) {
	cli := require_docker(t)
	pull_image(t, cli)
	server := fake_release(t, "9.9.9", []byte(fake_claude))

	tmp := t.TempDir()
	cfg := &config.Config{
		CacheDir: filepath.Join(tmp, "cache"),
		DataDir:  filepath.Join(tmp, "data"),
		Docker:   config.DockerConfig{Mode: config.DockerModeOff},
		Release:  config.ReleaseConfig{BaseURL: server.URL, Channel: "stable", CheckInterval: config.Duration(time.Hour)},
		Sync:     config.SyncConfig{Debounce: config.Duration(200 * time.Millisecond), FallbackInterval: config.Duration(time.Minute)},
	}
	require.NoError(t, os.MkdirAll(cfg.CacheDir, 0o755))

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

	lf := "/tmp/otlp-" + strings.ReplaceAll(t.Name(), "/", "-")
	name := devc.DisplayName(lf)
	ctr := run_devcontainer(t, cli, lf)

	wait_for(t, 60*time.Second, "ready", func() bool {
		it := find_item(must_items(t, cfg), name)
		return it != nil && it.Status == StatusReady
	})

	// Before any export the container must report nothing at all — a nil, not a
	// zero, so the listing can tell "never measured" from "measured, idle".
	require.Nil(t, find_item(must_items(t, cfg), name).Telemetry,
		"a container that has not exported yet carries no telemetry")

	// Two separate exports, so the assertion also covers delta accumulation
	// across round trips rather than a single body being read correctly.
	post := func(input, output int, cost float64) {
		t.Helper()
		body := fmt.Sprintf(`{"resourceMetrics":[{"scopeMetrics":[{"metrics":[
			{"name":"claude_code.token.usage","unit":"tokens","sum":{"aggregationTemporality":1,"isMonotonic":true,
				"dataPoints":[
					{"asInt":"%d","attributes":[{"key":"type","value":{"stringValue":"input"}}]},
					{"asInt":"%d","attributes":[{"key":"type","value":{"stringValue":"output"}}]}
				]}},
			{"name":"claude_code.cost.usage","unit":"USD","sum":{"aggregationTemporality":1,"isMonotonic":true,
				"dataPoints":[{"asDouble":%f}]}}
		]}]}]}`, input, output, cost)

		// busybox wget is present in the alpine-based e2e image; POST the body
		// exactly where session_env tells claude to send it.
		var out string
		var code int
		var err error
		wait_for(t, 30*time.Second, "otlp endpoint reachable in container", func() bool {
			out, code, err = dockerx.ExecOutput(t.Context(), cli, ctr, "root", []string{
				"sh", "-c", fmt.Sprintf(
					"wget -q -O- --header='Content-Type: application/json' --post-data=%s http://%s/v1/metrics",
					shellQuote(strings.Join(strings.Fields(body), "")), otlpListenAddr),
			})
			return err == nil && code == 0
		})
		require.NoError(t, err)
		require.Equalf(t, 0, code, "wget failed: %s", out)
	}

	post(1000, 200, 0.05)
	post(500, 100, 0.02)

	var tel *Telemetry
	wait_for(t, 20*time.Second, "telemetry visible in the listing", func() bool {
		it := find_item(must_items(t, cfg), name)
		if it == nil || it.Telemetry == nil {
			return false
		}
		tel = it.Telemetry
		return tel.Tokens == 1800
	})

	require.NotNil(t, tel, "the posted metrics must reach the listing")
	require.EqualValues(t, 1800, tel.Tokens, "both exports accumulate")
	require.EqualValues(t, 1500, tel.ByType["input"])
	require.EqualValues(t, 300, tel.ByType["output"])
	require.InDelta(t, 0.07, tel.CostUSD, 1e-6)
	require.Positive(t, tel.TokensPerMin, "a container that just spent tokens has a live rate")

	t.Run("session env points claude at the relay", func(t *testing.T) {
		// The endpoint the container was actually given must be the one the
		// receiver listens on — the two are wired independently, and a mismatch
		// would leave claude's exporter retrying into a closed port forever with
		// no visible symptom other than permanently empty totals.
		d.mu.Lock()
		e := d.entries[ctr]
		d.mu.Unlock()
		require.NotNil(t, e)

		env := envMap(d.session_env(e).Overrides())
		require.Equal(t, "http://"+otlpListenAddr, env["OTEL_EXPORTER_OTLP_ENDPOINT"])
		require.Equal(t, "1", env["CLAUDE_CODE_ENABLE_TELEMETRY"])
	})
}

// shellQuote wraps s in single quotes for `sh -c`. The OTLP bodies here contain
// no single quote, so the simple form is sufficient and the helper asserts that
// rather than implementing full escaping.
func shellQuote(s string) string {
	if strings.Contains(s, "'") {
		panic("shellQuote: unsupported single quote in payload")
	}
	return "'" + s + "'"
}
