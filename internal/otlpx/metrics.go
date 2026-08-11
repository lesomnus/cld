// Package otlpx decodes the OTLP/HTTP **JSON** encoding of a metrics export
// into a flat list of data points. It is deliberately not a general OTLP
// implementation: cld only needs to read the handful of counters Claude Code
// emits (see internal/daemon/telemetry.go), and the JSON protocol lets that be
// done with encoding/json alone — no protobuf runtime, no collector dependency.
//
// The encoding follows the proto3 JSON mapping, whose two traps this package
// exists to absorb:
//
//   - 64-bit integers are encoded as JSON *strings* ("1234"), while doubles are
//     bare numbers. A field is therefore parsed from its raw bytes rather than
//     into a fixed Go numeric type, so either spelling decodes.
//   - Every field is optional. A metric carrying no recognized data (an
//     unset sum, an empty point list, a value that is neither asInt nor
//     asDouble) is skipped rather than treated as zero, so a producer's
//     omission never lands in a total as a real 0.
//
// Unknown metrics, unknown point kinds (histograms), and unknown attribute
// value types are ignored on purpose: Claude Code keeps adding instrumentation,
// and a new metric must never break decoding of the ones cld reads.
package otlpx

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
)

// Temporality is how a sum's value relates to previous exports.
type Temporality int

const (
	// TemporalityUnspecified is the proto default, used when the producer did
	// not say. Treated as unknown by callers, never silently as delta.
	TemporalityUnspecified Temporality = 0
	// TemporalityDelta means the value covers only the interval since the last
	// export, so consecutive exports are summed. Claude Code's default.
	TemporalityDelta Temporality = 1
	// TemporalityCumulative means the value is a running total since process
	// start, so consecutive exports replace rather than add.
	TemporalityCumulative Temporality = 2
)

// Point is one decoded numeric data point, flattened out of the
// resource/scope/metric nesting. Attrs merges the resource-level attributes
// with the point's own; the point's win on a key collision, since they describe
// the measurement rather than the producer.
type Point struct {
	Metric      string
	Unit        string
	Attrs       map[string]string
	Value       float64
	Temporality Temporality
	// Monotonic reports the sum's isMonotonic flag. A non-monotonic sum can go
	// down, so a caller accumulating counters should skip it.
	Monotonic bool
}

// Attr returns the attribute for key, or "" when absent.
func (p Point) Attr(key string) string { return p.Attrs[key] }

// DecodeMetrics parses an OTLP/JSON ExportMetricsServiceRequest body into flat
// points. A body that is not a JSON object fails; anything inside it that is
// unrecognized is skipped, so a partially understood export still yields the
// points cld does understand.
func DecodeMetrics(raw []byte) ([]Point, error) {
	var req exportMetricsRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("decode otlp metrics: %w", err)
	}

	var out []Point
	for _, rm := range req.ResourceMetrics {
		base := attrMap(rm.Resource.Attributes, nil)
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				out = appendMetric(out, m, base)
			}
		}
	}
	return out, nil
}

// appendMetric flattens one metric's data points. Only sums and gauges are
// read — the two shapes Claude Code's counters use; histograms and exponential
// histograms are skipped rather than approximated.
func appendMetric(out []Point, m metric, base map[string]string) []Point {
	switch {
	case m.Sum != nil:
		for _, dp := range m.Sum.DataPoints {
			v, ok := dp.value()
			if !ok {
				continue
			}
			out = append(out, Point{
				Metric:      m.Name,
				Unit:        m.Unit,
				Attrs:       attrMap(dp.Attributes, base),
				Value:       v,
				Temporality: Temporality(m.Sum.AggregationTemporality),
				Monotonic:   m.Sum.IsMonotonic,
			})
		}
	case m.Gauge != nil:
		for _, dp := range m.Gauge.DataPoints {
			v, ok := dp.value()
			if !ok {
				continue
			}
			out = append(out, Point{
				Metric: m.Name,
				Unit:   m.Unit,
				Attrs:  attrMap(dp.Attributes, base),
				Value:  v,
			})
		}
	}
	return out
}

// attrMap renders an attribute list as a string map, layered over base (which
// is never mutated). Only scalar values are kept; arrays and nested maps are
// dropped, since nothing cld reads uses them.
func attrMap(attrs []keyValue, base map[string]string) map[string]string {
	if len(attrs) == 0 && len(base) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(attrs))
	maps.Copy(out, base)
	for _, a := range attrs {
		if s, ok := a.Value.str(); ok {
			out[a.Key] = s
		}
	}
	return out
}

type exportMetricsRequest struct {
	ResourceMetrics []resourceMetrics `json:"resourceMetrics"`
}

type resourceMetrics struct {
	Resource     resource       `json:"resource"`
	ScopeMetrics []scopeMetrics `json:"scopeMetrics"`
}

type resource struct {
	Attributes []keyValue `json:"attributes"`
}

type scopeMetrics struct {
	Metrics []metric `json:"metrics"`
}

type metric struct {
	Name  string `json:"name"`
	Unit  string `json:"unit"`
	Sum   *sum   `json:"sum"`
	Gauge *gauge `json:"gauge"`
}

type sum struct {
	DataPoints             []numberDataPoint `json:"dataPoints"`
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic"`
}

type gauge struct {
	DataPoints []numberDataPoint `json:"dataPoints"`
}

type numberDataPoint struct {
	Attributes []keyValue      `json:"attributes"`
	AsDouble   json.RawMessage `json:"asDouble"`
	AsInt      json.RawMessage `json:"asInt"`
}

// value reads the point's numeric value from whichever of the two mutually
// exclusive fields the producer used. ok is false when neither is present or
// neither parses, so the caller skips the point instead of recording a 0.
func (p numberDataPoint) value() (float64, bool) {
	if v, ok := parseNumber(p.AsDouble); ok {
		return v, true
	}
	return parseNumber(p.AsInt)
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

// anyValue is the subset of OTLP's AnyValue cld reads: the scalars. Every
// variant is captured raw so an int64-as-string decodes the same as a number.
type anyValue struct {
	StringValue json.RawMessage `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"`
	DoubleValue json.RawMessage `json:"doubleValue"`
	BoolValue   json.RawMessage `json:"boolValue"`
}

// str renders a scalar attribute value as a string. Numbers are formatted
// without a trailing ".0" so an int-valued double reads like the integer it is.
// ok is false for an absent or non-scalar value.
func (v anyValue) str() (string, bool) {
	if len(v.StringValue) > 0 {
		var s string
		if json.Unmarshal(v.StringValue, &s) == nil {
			return s, true
		}
	}
	if n, ok := parseNumber(v.IntValue); ok {
		return strconv.FormatInt(int64(n), 10), true
	}
	if n, ok := parseNumber(v.DoubleValue); ok {
		return strconv.FormatFloat(n, 'f', -1, 64), true
	}
	if len(v.BoolValue) > 0 {
		var b bool
		if json.Unmarshal(v.BoolValue, &b) == nil {
			return strconv.FormatBool(b), true
		}
	}
	return "", false
}

// parseNumber reads a JSON number that proto3 may have encoded either bare
// (doubles) or quoted (64-bit ints). An absent, null, or unparseable field
// yields ok=false — never a silent 0.
func parseNumber(raw json.RawMessage) (float64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
