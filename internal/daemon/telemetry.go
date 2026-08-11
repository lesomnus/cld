package daemon

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/lesomnus/cld/internal/otlpx"
)

// Claude Code's two consumption counters. Both are delta sums incremented after
// each API request, so a session that is mid-generation reports nothing until
// its request completes — the reason the listing's numbers step up per turn
// rather than flowing (see telemetryExportInterval).
const (
	metricTokenUsage = "claude_code.token.usage"
	metricCostUsage  = "claude_code.cost.usage"
)

// The `type` attribute values claude tags each token.usage point with. input is
// fresh prompt tokens, output the generated reply; cacheRead is prompt served
// cheaply from the prompt cache, and cacheCreation is prompt WRITTEN to the
// cache at a premium. cacheCreation is the one worth watching: cacheRead only
// ever grows as a long context is reused (a healthy, unactionable number),
// while a cacheCreation that keeps climbing turn after turn means the cache is
// being rebuilt repeatedly — the prefix changed or the entry expired — and the
// premium is being paid again and again.
const (
	TokenTypeInput         = "input"
	TokenTypeOutput        = "output"
	TokenTypeCacheRead     = "cacheRead"
	TokenTypeCacheCreation = "cacheCreation"
)

// telemetryRateWindow is how far back the per-minute consumption rate looks.
// Long enough that a single quiet turn does not read as "stopped", short enough
// that the number still tracks what the session is doing now.
const telemetryRateWindow = 5 * time.Minute

// telemetryRateFloor is the smallest denominator the rate is divided by. Within
// the first minute of a container's life — or after a single large export — the
// true elapsed time is tiny, and dividing by it would render an absurd spike.
// Flooring at a minute makes the figure read as "tokens in the last minute",
// which is exactly what it claims to be.
const telemetryRateFloor = time.Minute

// telemetryMaxBody caps one export body. Claude Code's metric payloads are a
// few KB; the limit is a guard against a container streaming an unbounded body
// into the daemon's memory through the relay, not a real operating bound.
const telemetryMaxBody = 4 << 20 // 4 MiB

// Telemetry is one container's accumulated Claude Code consumption, as reported
// by claude's own OpenTelemetry metrics over the in-container OTLP relay.
//
// It is scoped to what the daemon has WITNESSED: accumulation starts when the
// container's relay first receives an export and resets when the container goes
// away (see telemetryStore.drop). It is not a billing record and does not
// survive a daemon restart — for account-wide quota, see UsageReport, which is
// a different measurement entirely (that one covers every client on the
// account, including claude run outside cld).
type Telemetry struct {
	// CostUSD is claude's own cost estimate, summed across every API request.
	// Claude Code derives it from published per-model pricing, so it is an
	// ESTIMATE — and on a subscription plan a notional "what this would have
	// cost through the API" figure rather than anything billed.
	CostUSD float64 `json:"cost_usd"`
	// Tokens is every token type summed: the one number that answers "how much
	// did this container consume".
	Tokens int64 `json:"tokens"`
	// ByType breaks Tokens down by the metric's `type` attribute — input,
	// output, cacheRead, cacheCreation.
	ByType map[string]int64 `json:"by_type,omitempty"`
	// TokensPerMin is recent consumption over telemetryRateWindow. Unlike the
	// two totals it falls back toward zero once a session goes quiet, so it
	// reads as a live rate rather than a lifetime figure.
	TokensPerMin float64 `json:"tokens_per_min"`
	// CacheCreationPerMin is the same rolling rate restricted to cacheCreation
	// tokens: how fast the session is (re)writing the prompt cache right now. A
	// rate that stays high is the live signal that the cache keeps churning
	// rather than being written once and read back cheaply.
	CacheCreationPerMin float64 `json:"cache_creation_per_min"`
	// Since is when the first export arrived, UpdatedAt when the last did.
	Since     time.Time `json:"since,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// telemetrySample is one export's token count, kept only long enough to compute
// the rolling rate.
type telemetrySample struct {
	at     time.Time
	tokens int64
}

// telemetryEntry is one container's running totals plus the recent samples the
// rate is computed from.
type telemetryEntry struct {
	cost    float64
	tokens  int64
	byType  map[string]int64
	since   time.Time
	updated time.Time
	samples []telemetrySample
	// ccSamples mirrors samples but counts only cacheCreation tokens, so its
	// rate answers "is the cache still churning" independent of overall volume.
	ccSamples []telemetrySample
}

// telemetryStore accumulates per-container consumption. It is keyed by
// container id — bound daemon-side from the relay the export arrived on, never
// read from the payload — so a container can only ever add to its OWN totals,
// and the resource attributes claude ships (account uuid, email, session id)
// are not needed and not trusted for attribution.
//
// Every method tolerates a nil receiver, reading as "nothing was ever
// collected". A Daemon assembled field-by-field rather than through New — as
// the unit tests for the unrelated parts of the control plane do — would
// otherwise panic in Items() merely for listing.
type telemetryStore struct {
	mu  sync.Mutex
	per map[string]*telemetryEntry
	now func() time.Time // overridable in tests
}

func newTelemetryStore() *telemetryStore {
	return &telemetryStore{per: map[string]*telemetryEntry{}, now: time.Now}
}

// add folds one export's points into the container's totals. Only delta sums
// are accumulated: a cumulative sum is a running total that would be
// double-counted by adding it, and a non-monotonic sum can decrease. Claude
// Code exports delta monotonic sums, so anything else is a producer cld does
// not understand and is skipped rather than guessed at.
// tokenDelta is one export's per-type token counts, returned from add so the
// caller can also fold them into the persistent weekly aggregate.
type tokenDelta struct{ input, output, cacheCreation int64 }

func (s *telemetryStore) add(id string, points []otlpx.Point) tokenDelta {
	if s == nil || id == "" || len(points) == 0 {
		return tokenDelta{}
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.per[id]
	if e == nil {
		e = &telemetryEntry{byType: map[string]int64{}, since: now}
		s.per[id] = e
	}

	var tokens int64
	var d tokenDelta
	for _, p := range points {
		if p.Temporality != otlpx.TemporalityDelta || !p.Monotonic {
			continue
		}
		switch p.Metric {
		case metricTokenUsage:
			n := int64(p.Value)
			if n <= 0 {
				continue
			}
			tokens += n
			if t := p.Attr("type"); t != "" {
				e.byType[t] += n
				switch t {
				case TokenTypeInput:
					d.input += n
				case TokenTypeOutput:
					d.output += n
				case TokenTypeCacheCreation:
					d.cacheCreation += n
				}
			}
		case metricCostUsage:
			if p.Value > 0 {
				e.cost += p.Value
			}
		}
	}
	cacheCreation := d.cacheCreation

	// Stamp the export even when it carried no tokens (a cost-only round, or a
	// metric cld ignores): it still proves the relay is alive and the container
	// is instrumented, which is what distinguishes "0 so far" from "never
	// reported" in the listing.
	e.updated = now
	if tokens > 0 {
		e.tokens += tokens
		e.samples = append(e.samples, telemetrySample{at: now, tokens: tokens})
		e.samples = trimSamples(e.samples, now.Add(-telemetryRateWindow))
	}
	if cacheCreation > 0 {
		e.ccSamples = append(e.ccSamples, telemetrySample{at: now, tokens: cacheCreation})
		e.ccSamples = trimSamples(e.ccSamples, now.Add(-telemetryRateWindow))
	}
	return d
}

// trimSamples drops samples older than cutoff. The slice is kept in arrival
// order, so the cut is a prefix and the retained tail is reused in place.
func trimSamples(samples []telemetrySample, cutoff time.Time) []telemetrySample {
	i := 0
	for i < len(samples) && samples[i].at.Before(cutoff) {
		i++
	}
	if i == 0 {
		return samples
	}
	return append(samples[:0], samples[i:]...)
}

// get returns a container's accumulated telemetry, or nil when it has never
// reported. The nil is meaningful: the listing renders nothing at all for such
// a container rather than a zero, since "$0.00" would wrongly claim the
// container was measured and found idle.
func (s *telemetryStore) get(id string) *Telemetry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.per[id]
	if e == nil {
		return nil
	}
	now := s.now()

	t := &Telemetry{
		CostUSD:             e.cost,
		Tokens:              e.tokens,
		Since:               e.since,
		UpdatedAt:           e.updated,
		TokensPerMin:        rateOf(e.samples, e.since, now),
		CacheCreationPerMin: rateOf(e.ccSamples, e.since, now),
	}
	if len(e.byType) > 0 {
		t.ByType = make(map[string]int64, len(e.byType))
		for k, v := range e.byType {
			t.ByType[k] = v
		}
	}
	return t
}

// rateOf computes tokens-per-minute over the samples still inside the rate
// window. The denominator is the observed span — capped at the window so an
// old container is not averaged over its whole life, and floored at
// telemetryRateFloor so a young one does not spike (see that constant).
func rateOf(samples []telemetrySample, since, now time.Time) float64 {
	cutoff := now.Add(-telemetryRateWindow)
	var sum int64
	for _, s := range samples {
		if s.at.Before(cutoff) {
			continue
		}
		sum += s.tokens
	}
	if sum == 0 {
		return 0
	}

	span := now.Sub(since)
	if span > telemetryRateWindow {
		span = telemetryRateWindow
	}
	if span < telemetryRateFloor {
		span = telemetryRateFloor
	}
	return float64(sum) / span.Minutes()
}

// drop forgets a container's totals. Called when the daemon stops tracking the
// container, so a recreated one starts from zero rather than inheriting the
// consumption of the container that previously held its id.
func (s *telemetryStore) drop(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.per, id)
	s.mu.Unlock()
}

// ids lists the containers with accumulated telemetry, sorted, for tests and
// diagnostics.
func (s *telemetryStore) ids() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.per))
	for id := range s.per {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// otlp_api is the OTLP receiver served to ONE container over its own relay. It
// implements just the metrics half of OTLP/HTTP, and only the JSON encoding
// (see internal/otlpx) — which is what session_env points claude at.
//
// The container's identity is bound here, exactly as scoped_api binds it: every
// point lands under self_id no matter what the payload claims, so one
// container's export can never be attributed to another project.
func (d *Daemon) otlp_api(self_id string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, telemetryMaxBody))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		points, err := otlpx.DecodeMetrics(raw)
		if err != nil {
			// A body cld cannot parse is the producer's problem, but answering
			// non-2xx would make claude's exporter retry it forever. Accept and
			// drop, and leave a trace for diagnosis.
			d.log.Debug("otlp: undecodable metrics export",
				slog.String("id", short(self_id)), slog.String("error", err.Error()))
			otlpAccepted(w)
			return
		}
		delta := d.tel.add(self_id, points)
		// Fold the same deltas into the persistent, weekly-resetting aggregate
		// shown at the bottom of `cld watch`. The per-container store above is
		// forgotten when a container goes away; this one is not.
		d.week.add(delta.input, delta.output, delta.cacheCreation)
		otlpAccepted(w)
	})
	// OTLP/HTTP defines signal-specific paths; cld only receives metrics.
	// Accepting (and discarding) the other two keeps a misconfigured exporter
	// from retrying forever, though session_env turns both off at the source.
	mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, r *http.Request) { otlpAccepted(w) })
	mux.HandleFunc("POST /v1/traces", func(w http.ResponseWriter, r *http.Request) { otlpAccepted(w) })
	return mux
}

// otlpAccepted writes the empty ExportMetricsServiceResponse that signals full
// success. OTLP/HTTP requires a body — an empty one, not a bare 200 with no
// content — so the exporter does not treat the reply as malformed.
func otlpAccepted(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(struct{}{})
}
