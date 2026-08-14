package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// envMap turns session_env's KEY=VALUE list into a map for assertions.
func envMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			out[k] = v
		}
	}
	return out
}

// Telemetry is on by default: an arch-matching session must be pointed at the
// daemon's own receiver, with metrics only.
func TestSessionEnvTelemetryDefaultOn(t *testing.T) {
	d, _ := newTestDaemon(t)
	env := envMap(d.session_env(&entry{arch_ok: true}).Overrides())

	require.Equal(t, "1", env["CLAUDE_CODE_ENABLE_TELEMETRY"])
	require.Equal(t, "otlp", env["OTEL_METRICS_EXPORTER"])
	require.Equal(t, "http://"+otlpListenAddr, env["OTEL_EXPORTER_OTLP_ENDPOINT"])

	// JSON, because internal/otlpx decodes only that — protobuf would be
	// received and silently dropped.
	require.Equal(t, "http/json", env["OTEL_EXPORTER_OTLP_PROTOCOL"])

	// Logs and traces carry prompt and tool metadata cld has no use for; they
	// must be off at the source rather than received and discarded.
	require.Equal(t, "none", env["OTEL_LOGS_EXPORTER"])
	require.Equal(t, "none", env["OTEL_TRACES_EXPORTER"])

	// Attribution is bound daemon-side by the relay, so the identifying
	// resource attributes must not be shipped at all.
	require.Equal(t, "false", env["OTEL_METRICS_INCLUDE_SESSION_ID"])
	require.Equal(t, "false", env["OTEL_METRICS_INCLUDE_ACCOUNT_UUID"])
}

// The interval must be pushed explicitly: Claude Code's own default is 60s,
// which is far too laggy for a listing.
func TestSessionEnvTelemetryInterval(t *testing.T) {
	d, cfg := newTestDaemon(t)

	env := envMap(d.session_env(&entry{arch_ok: true}).Overrides())
	require.Equal(t, "5000", env["OTEL_METRIC_EXPORT_INTERVAL"])

	cfg.Telemetry.ExportInterval = config.Duration(20 * time.Second)
	env = envMap(d.session_env(&entry{arch_ok: true}).Overrides())
	require.Equal(t, "20000", env["OTEL_METRIC_EXPORT_INTERVAL"])
}

// Disabling telemetry must leave the session's environment untouched — not
// merely point it somewhere inert.
func TestSessionEnvTelemetryDisabled(t *testing.T) {
	d, cfg := newTestDaemon(t)
	cfg.Telemetry.Disabled = true

	for _, kv := range d.session_env(&entry{arch_ok: true}).Overrides() {
		require.NotContains(t, kv, "OTEL_")
		require.NotContains(t, kv, "CLAUDE_CODE_ENABLE_TELEMETRY")
	}
}

// A cross-arch container cannot run cld's binary, so relay_otlp never serves
// there. Pointing claude at a dead port would leave its exporter retrying for
// the life of the session, so the env must be omitted entirely.
func TestSessionEnvTelemetryNeedsRelay(t *testing.T) {
	d, _ := newTestDaemon(t)
	require.False(t, d.telemetry_session(&entry{arch_ok: false}))

	for _, kv := range d.session_env(&entry{arch_ok: false}).Overrides() {
		require.NotContains(t, kv, "OTEL_")
	}
}

// The receiver and the auth proxy share the relay mechanism but not a port.
func TestOtlpAndProxyPortsDiffer(t *testing.T) {
	require.NotEqual(t, proxyListenAddr, otlpListenAddr)
}
