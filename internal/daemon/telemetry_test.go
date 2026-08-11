package daemon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/cld/internal/otlpx"
	"github.com/stretchr/testify/require"
)

// clock is a hand-driven time source so the rate window can be exercised
// without sleeping.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestStore() (*telemetryStore, *clock) {
	c := &clock{t: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)}
	s := newTelemetryStore()
	s.now = c.now
	return s, c
}

// tokenPoint builds a delta monotonic token point, the shape Claude Code sends.
func tokenPoint(kind string, n float64) otlpx.Point {
	return otlpx.Point{
		Metric:      metricTokenUsage,
		Attrs:       map[string]string{"type": kind},
		Value:       n,
		Temporality: otlpx.TemporalityDelta,
		Monotonic:   true,
	}
}

func costPoint(usd float64) otlpx.Point {
	return otlpx.Point{
		Metric:      metricCostUsage,
		Value:       usd,
		Temporality: otlpx.TemporalityDelta,
		Monotonic:   true,
	}
}

// Successive delta exports accumulate rather than replace, and the per-type
// breakdown tracks the total.
func TestTelemetryStoreAccumulates(t *testing.T) {
	s, c := newTestStore()

	s.add("ctr", []otlpx.Point{tokenPoint("input", 1000), tokenPoint("output", 200), costPoint(0.05)})
	c.add(10 * time.Second)
	s.add("ctr", []otlpx.Point{tokenPoint("input", 500), tokenPoint("cacheRead", 3000), costPoint(0.02)})

	got := s.get("ctr")
	require.NotNil(t, got)
	require.EqualValues(t, 4700, got.Tokens)
	require.InDelta(t, 0.07, got.CostUSD, 1e-9)
	require.EqualValues(t, 1500, got.ByType["input"])
	require.EqualValues(t, 200, got.ByType["output"])
	require.EqualValues(t, 3000, got.ByType["cacheRead"])
}

// A container that never exported must read as nil, not as a measured zero:
// the listing renders nothing for nil and would otherwise claim the container
// was watched and found idle.
func TestTelemetryStoreUnreportedIsNil(t *testing.T) {
	s, _ := newTestStore()
	require.Nil(t, s.get("never-reported"))
}

// An export carrying nothing cld reads still marks the container as reporting —
// that is what distinguishes "instrumented, nothing spent yet" from "never
// reported".
func TestTelemetryStoreEmptyExportStillRegisters(t *testing.T) {
	s, c := newTestStore()
	s.add("ctr", []otlpx.Point{{Metric: "claude_code.session.count", Value: 1,
		Temporality: otlpx.TemporalityDelta, Monotonic: true}})

	got := s.get("ctr")
	require.NotNil(t, got)
	require.EqualValues(t, 0, got.Tokens)
	require.Equal(t, c.t, got.UpdatedAt)
}

// Only delta monotonic sums may be accumulated. A cumulative sum is a running
// total that adding would double-count, and a non-monotonic one can decrease;
// both must be skipped rather than guessed at.
func TestTelemetryStoreSkipsNonDeltaPoints(t *testing.T) {
	s, _ := newTestStore()

	cumulative := tokenPoint("input", 5000)
	cumulative.Temporality = otlpx.TemporalityCumulative
	unspecified := tokenPoint("input", 5000)
	unspecified.Temporality = otlpx.TemporalityUnspecified
	nonMonotonic := tokenPoint("input", 5000)
	nonMonotonic.Monotonic = false

	s.add("ctr", []otlpx.Point{cumulative, unspecified, nonMonotonic, tokenPoint("input", 10)})

	got := s.get("ctr")
	require.NotNil(t, got)
	require.EqualValues(t, 10, got.Tokens, "only the delta monotonic point counts")
}

// Totals are per container: one container's export must never land on another.
func TestTelemetryStoreIsolatesContainers(t *testing.T) {
	s, _ := newTestStore()
	s.add("a", []otlpx.Point{tokenPoint("input", 100)})
	s.add("b", []otlpx.Point{tokenPoint("input", 7)})

	require.EqualValues(t, 100, s.get("a").Tokens)
	require.EqualValues(t, 7, s.get("b").Tokens)
	require.Equal(t, []string{"a", "b"}, s.ids())
}

// drop forgets a container so a recreated one starting on the same id does not
// inherit its predecessor's totals.
func TestTelemetryStoreDrop(t *testing.T) {
	s, _ := newTestStore()
	s.add("ctr", []otlpx.Point{tokenPoint("input", 100)})
	require.NotNil(t, s.get("ctr"))

	s.drop("ctr")
	require.Nil(t, s.get("ctr"))
	require.Empty(t, s.ids())

	s.add("ctr", []otlpx.Point{tokenPoint("input", 5)})
	require.EqualValues(t, 5, s.get("ctr").Tokens)
}

// Within the first minute the rate is divided by the floor, not by the tiny
// real elapsed time — otherwise a single export would render an absurd spike.
func TestTelemetryRateFlooredWhenYoung(t *testing.T) {
	s, c := newTestStore()
	s.add("ctr", []otlpx.Point{tokenPoint("input", 6000)})
	c.add(2 * time.Second)

	got := s.get("ctr")
	require.InDelta(t, 6000, got.TokensPerMin, 1e-6,
		"6000 tokens 2s in must read as 6000/min, not 180000/min")
}

// Past the floor the rate is a genuine average over the observed span.
func TestTelemetryRateAveragesOverSpan(t *testing.T) {
	s, c := newTestStore()
	s.add("ctr", []otlpx.Point{tokenPoint("input", 6000)})
	c.add(2 * time.Minute)
	s.add("ctr", []otlpx.Point{tokenPoint("input", 6000)})

	// 12000 tokens over a 2-minute span.
	require.InDelta(t, 6000, s.get("ctr").TokensPerMin, 1e-6)
}

// Samples older than the window stop counting, so the rate falls back toward
// zero once a session goes quiet — while the lifetime totals stay put.
func TestTelemetryRateDecaysButTotalsPersist(t *testing.T) {
	s, c := newTestStore()
	s.add("ctr", []otlpx.Point{tokenPoint("input", 9000), costPoint(0.5)})

	c.add(telemetryRateWindow + time.Minute)
	got := s.get("ctr")
	require.Zero(t, got.TokensPerMin, "a session quiet longer than the window reports no rate")
	require.EqualValues(t, 9000, got.Tokens, "the lifetime total does not decay")
	require.InDelta(t, 0.5, got.CostUSD, 1e-9)
}

// The retained sample slice must not grow without bound as a long-lived
// container keeps exporting.
func TestTelemetryStoreTrimsSamples(t *testing.T) {
	s, c := newTestStore()
	for range 200 {
		s.add("ctr", []otlpx.Point{tokenPoint("input", 1)})
		c.add(10 * time.Second)
	}

	s.mu.Lock()
	n := len(s.per["ctr"].samples)
	s.mu.Unlock()
	// 5-minute window at one sample per 10s holds ~30, never all 200.
	require.LessOrEqual(t, n, 32)
	require.EqualValues(t, 200, s.get("ctr").Tokens)
}

// The mutated map returned by get must be a copy: a caller editing it must not
// corrupt the store's own totals.
func TestTelemetryGetReturnsCopy(t *testing.T) {
	s, _ := newTestStore()
	s.add("ctr", []otlpx.Point{tokenPoint("input", 100)})

	got := s.get("ctr")
	got.ByType["input"] = 999999

	require.EqualValues(t, 100, s.get("ctr").ByType["input"])
}

func testDaemon() *Daemon {
	return &Daemon{
		log: slog.New(slog.DiscardHandler),
		tel: newTelemetryStore(),
	}
}

// The receiver attributes every export to the container its relay was built
// for. This is the security property: the payload's own identity attributes are
// never consulted, so a container cannot write into another project's totals.
func TestOtlpAPIBindsContainerIdentity(t *testing.T) {
	d := testDaemon()
	srv := httptest.NewServer(d.otlp_api("real-container"))
	defer srv.Close()

	// The body claims to be a different container in every way it can.
	body := `{"resourceMetrics":[{
		"resource":{"attributes":[
			{"key":"container.id","value":{"stringValue":"someone-elses-container"}},
			{"key":"session.id","value":{"stringValue":"forged"}}
		]},
		"scopeMetrics":[{"metrics":[{"name":"claude_code.token.usage",
			"sum":{"aggregationTemporality":1,"isMonotonic":true,
				"dataPoints":[{"asInt":"1234","attributes":[{"key":"type","value":{"stringValue":"input"}}]}]}}]}]
	}]}`

	res, err := http.Post(srv.URL+"/v1/metrics", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)

	require.Equal(t, []string{"real-container"}, d.tel.ids())
	require.EqualValues(t, 1234, d.tel.get("real-container").Tokens)
	require.Nil(t, d.tel.get("someone-elses-container"))
}

// OTLP/HTTP requires a JSON body on success, not a bare 200 — an exporter
// treats a missing one as a malformed reply and retries.
func TestOtlpAPIRespondsWithEmptyJSON(t *testing.T) {
	d := testDaemon()
	srv := httptest.NewServer(d.otlp_api("ctr"))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/metrics", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "application/json", res.Header.Get("Content-Type"))
	var doc map[string]any
	require.NoError(t, json.NewDecoder(res.Body).Decode(&doc))
	require.Empty(t, doc)
}

// An undecodable body must be accepted and dropped. Answering non-2xx would
// make claude's exporter retry the same broken payload for the life of the
// session.
func TestOtlpAPIAcceptsUndecodableBody(t *testing.T) {
	d := testDaemon()
	srv := httptest.NewServer(d.otlp_api("ctr"))
	defer srv.Close()

	res, err := http.Post(srv.URL+"/v1/metrics", "application/json", strings.NewReader("this is not json"))
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Nil(t, d.tel.get("ctr"), "a body that did not decode contributes nothing")
}

// The other OTLP signals are accepted and discarded, so an exporter configured
// beyond what session_env sets does not retry forever.
func TestOtlpAPIAcceptsOtherSignals(t *testing.T) {
	d := testDaemon()
	srv := httptest.NewServer(d.otlp_api("ctr"))
	defer srv.Close()

	for _, path := range []string{"/v1/logs", "/v1/traces"} {
		res, err := http.Post(srv.URL+path, "application/json", strings.NewReader(`{}`))
		require.NoError(t, err)
		res.Body.Close()
		require.Equalf(t, http.StatusOK, res.StatusCode, "POST %s", path)
	}
	require.Nil(t, d.tel.get("ctr"), "non-metric signals contribute nothing")
}

// A body past the cap is truncated by the reader rather than buffered whole.
// Truncation makes it undecodable, which the handler already absorbs — the
// point is that the daemon's memory is bounded regardless of what a container
// sends through the relay.
func TestOtlpAPILimitsBodySize(t *testing.T) {
	d := testDaemon()
	srv := httptest.NewServer(d.otlp_api("ctr"))
	defer srv.Close()

	huge := fmt.Sprintf(`{"resourceMetrics":[{"pad":%q}]}`, strings.Repeat("x", telemetryMaxBody+1024))
	res, err := http.Post(srv.URL+"/v1/metrics", "application/json", strings.NewReader(huge))
	require.NoError(t, err)
	defer res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
}

// A Daemon assembled field-by-field (as several control-plane unit tests do)
// has no store. Listing must still work rather than panicking — the container
// simply reads as never having reported.
func TestTelemetryStoreNilSafe(t *testing.T) {
	var s *telemetryStore
	require.NotPanics(t, func() {
		s.add("ctr", []otlpx.Point{tokenPoint("input", 1)})
		require.Nil(t, s.get("ctr"))
		require.Empty(t, s.ids())
		s.drop("ctr")
	})

	d := &Daemon{entries: map[string]*entry{}}
	e := &entry{id: "ctr"}
	e.item = Item{ID: "ctr", Name: "n"}
	e.publish()
	d.entries["ctr"] = e
	require.NotPanics(t, func() {
		items := d.Items()
		require.Len(t, items, 1)
		require.Nil(t, items[0].Telemetry)
	})
}
