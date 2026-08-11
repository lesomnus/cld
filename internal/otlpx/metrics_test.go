package otlpx

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// claudeExport is a metrics body shaped like the one Claude Code sends: two
// delta monotonic sums, with the token counts spelled as proto3 JSON int64s
// (quoted strings) and the cost as a bare double. Resource attributes sit a
// level above the points and must reach every point.
const claudeExport = `{
  "resourceMetrics": [{
    "resource": {"attributes": [
      {"key": "service.name", "value": {"stringValue": "claude-code"}},
      {"key": "user.email", "value": {"stringValue": "me@example.com"}}
    ]},
    "scopeMetrics": [{
      "scope": {"name": "com.anthropic.claude_code"},
      "metrics": [
        {
          "name": "claude_code.token.usage",
          "unit": "tokens",
          "sum": {
            "aggregationTemporality": 1,
            "isMonotonic": true,
            "dataPoints": [
              {"asInt": "1200", "attributes": [
                {"key": "type", "value": {"stringValue": "input"}},
                {"key": "model", "value": {"stringValue": "claude-opus-5"}}
              ]},
              {"asInt": "340", "attributes": [
                {"key": "type", "value": {"stringValue": "output"}},
                {"key": "model", "value": {"stringValue": "claude-opus-5"}}
              ]}
            ]
          }
        },
        {
          "name": "claude_code.cost.usage",
          "unit": "USD",
          "sum": {
            "aggregationTemporality": 1,
            "isMonotonic": true,
            "dataPoints": [
              {"asDouble": 0.0425, "attributes": [
                {"key": "model", "value": {"stringValue": "claude-opus-5"}}
              ]}
            ]
          }
        }
      ]
    }]
  }]
}`

func TestDecodeMetricsClaudeExport(t *testing.T) {
	points, err := DecodeMetrics([]byte(claudeExport))
	require.NoError(t, err)
	require.Len(t, points, 3)

	// int64-as-string must decode to the number, not be dropped or read as 0.
	require.Equal(t, "claude_code.token.usage", points[0].Metric)
	require.Equal(t, 1200.0, points[0].Value)
	require.Equal(t, "input", points[0].Attr("type"))
	require.Equal(t, TemporalityDelta, points[0].Temporality)
	require.True(t, points[0].Monotonic)
	require.Equal(t, "tokens", points[0].Unit)

	require.Equal(t, 340.0, points[1].Value)
	require.Equal(t, "output", points[1].Attr("type"))

	// A bare double decodes just the same.
	require.Equal(t, "claude_code.cost.usage", points[2].Metric)
	require.InDelta(t, 0.0425, points[2].Value, 1e-9)

	// Resource attributes reach every point, merged with the point's own.
	for _, p := range points {
		require.Equal(t, "claude-code", p.Attr("service.name"))
		require.Equal(t, "me@example.com", p.Attr("user.email"))
		require.Equal(t, "claude-opus-5", p.Attr("model"))
	}
}

// A point-level attribute must win over a resource-level one of the same key:
// it describes the measurement, not the producer.
func TestDecodeMetricsPointAttrsShadowResource(t *testing.T) {
	const body = `{"resourceMetrics":[{
		"resource":{"attributes":[{"key":"model","value":{"stringValue":"resource-level"}}]},
		"scopeMetrics":[{"metrics":[{"name":"m","sum":{"aggregationTemporality":1,"isMonotonic":true,
			"dataPoints":[{"asInt":"1","attributes":[{"key":"model","value":{"stringValue":"point-level"}}]}]}}]}]
	}]}`

	points, err := DecodeMetrics([]byte(body))
	require.NoError(t, err)
	require.Len(t, points, 1)
	require.Equal(t, "point-level", points[0].Attr("model"))
}

// Temporality and monotonicity must survive decoding unmangled — the caller
// relies on them to decide whether a value may be accumulated.
func TestDecodeMetricsTemporality(t *testing.T) {
	const body = `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[
		{"name":"cumulative","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[{"asInt":"5"}]}},
		{"name":"unset","sum":{"isMonotonic":true,"dataPoints":[{"asInt":"5"}]}},
		{"name":"nonmonotonic","sum":{"aggregationTemporality":1,"dataPoints":[{"asInt":"5"}]}}
	]}]}]}`

	points, err := DecodeMetrics([]byte(body))
	require.NoError(t, err)
	require.Len(t, points, 3)

	require.Equal(t, TemporalityCumulative, points[0].Temporality)
	require.True(t, points[0].Monotonic)
	require.Equal(t, TemporalityUnspecified, points[1].Temporality)
	require.Equal(t, TemporalityDelta, points[2].Temporality)
	require.False(t, points[2].Monotonic)
}

// A point with neither asInt nor asDouble carries no measurement. It must be
// skipped, never recorded as a real 0 that would dilute an average or claim a
// counter reported nothing.
func TestDecodeMetricsSkipsValuelessPoints(t *testing.T) {
	const body = `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[
		{"name":"m","sum":{"aggregationTemporality":1,"isMonotonic":true,"dataPoints":[
			{"attributes":[{"key":"type","value":{"stringValue":"input"}}]},
			{"asInt":"7"}
		]}}
	]}]}]}`

	points, err := DecodeMetrics([]byte(body))
	require.NoError(t, err)
	require.Len(t, points, 1)
	require.Equal(t, 7.0, points[0].Value)
}

// An explicit zero is a real measurement and must NOT be confused with the
// absent case above.
func TestDecodeMetricsKeepsExplicitZero(t *testing.T) {
	const body = `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[
		{"name":"m","sum":{"aggregationTemporality":1,"isMonotonic":true,"dataPoints":[{"asInt":"0"}]}}
	]}]}]}`

	points, err := DecodeMetrics([]byte(body))
	require.NoError(t, err)
	require.Len(t, points, 1)
	require.Equal(t, 0.0, points[0].Value)
}

// Shapes cld does not read (histograms) and metrics it has never seen must not
// break the export: the points it does understand still come through.
func TestDecodeMetricsIgnoresUnknownShapes(t *testing.T) {
	const body = `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[
		{"name":"hist","histogram":{"dataPoints":[{"count":"3","sum":1.5}]}},
		{"name":"exp","exponentialHistogram":{"dataPoints":[{"count":"1"}]}},
		{"name":"claude_code.token.usage","sum":{"aggregationTemporality":1,"isMonotonic":true,
			"dataPoints":[{"asInt":"42"}]}}
	]}]}]}`

	points, err := DecodeMetrics([]byte(body))
	require.NoError(t, err)
	require.Len(t, points, 1)
	require.Equal(t, "claude_code.token.usage", points[0].Metric)
	require.Equal(t, 42.0, points[0].Value)
}

// A gauge is read too, though Claude Code's consumption counters are sums.
func TestDecodeMetricsGauge(t *testing.T) {
	const body = `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[
		{"name":"g","gauge":{"dataPoints":[{"asDouble":2.5}]}}
	]}]}]}`

	points, err := DecodeMetrics([]byte(body))
	require.NoError(t, err)
	require.Len(t, points, 1)
	require.Equal(t, 2.5, points[0].Value)
	// A gauge has no temporality, so it must not read as delta — which is what
	// the accumulator keys on to decide a value may be summed.
	require.Equal(t, TemporalityUnspecified, points[0].Temporality)
	require.False(t, points[0].Monotonic)
}

// Non-string scalar attribute values still render, including an int64 spelled
// as a string.
func TestDecodeMetricsAttrValueTypes(t *testing.T) {
	const body = `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[
		{"name":"m","sum":{"aggregationTemporality":1,"isMonotonic":true,"dataPoints":[{"asInt":"1","attributes":[
			{"key":"i","value":{"intValue":"12"}},
			{"key":"d","value":{"doubleValue":1.5}},
			{"key":"b","value":{"boolValue":true}},
			{"key":"arr","value":{"arrayValue":{"values":[{"stringValue":"x"}]}}}
		]}]}}
	]}]}]}`

	points, err := DecodeMetrics([]byte(body))
	require.NoError(t, err)
	require.Len(t, points, 1)
	require.Equal(t, "12", points[0].Attr("i"))
	require.Equal(t, "1.5", points[0].Attr("d"))
	require.Equal(t, "true", points[0].Attr("b"))
	require.Equal(t, "", points[0].Attr("arr"), "non-scalar attribute values are dropped")
}

func TestDecodeMetricsMalformed(t *testing.T) {
	_, err := DecodeMetrics([]byte("not json"))
	require.Error(t, err)

	// A well-formed but empty envelope is not an error, just no points.
	points, err := DecodeMetrics([]byte(`{}`))
	require.NoError(t, err)
	require.Empty(t, points)
}
