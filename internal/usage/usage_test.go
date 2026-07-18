package usage_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lesomnus/cld/internal/usage"
	"github.com/stretchr/testify/require"
)

// sample is a trimmed copy of a real /api/oauth/usage response, including the
// unified limits array, a null scoped reset, and internal code-named buckets
// the parser must ignore.
const sample = `{
  "five_hour": {"utilization": 8.0, "resets_at": "2026-07-18T19:00:00.007362+00:00", "limit_dollars": null},
  "seven_day": {"utilization": 17.0, "resets_at": "2026-07-24T18:00:00.007386+00:00"},
  "seven_day_opus": null,
  "iguana_necktie": null,
  "limits": [
    {"kind": "session", "group": "session", "percent": 8, "severity": "normal", "resets_at": "2026-07-18T19:00:00.007362+00:00", "scope": null, "is_active": false},
    {"kind": "weekly_all", "group": "weekly", "percent": 17, "severity": "normal", "resets_at": "2026-07-24T18:00:00.007386+00:00", "scope": null, "is_active": true},
    {"kind": "weekly_scoped", "group": "weekly", "percent": 0, "severity": "normal", "resets_at": null, "scope": {"model": {"display_name": "Fable"}}, "is_active": false}
  ]
}`

func TestFetch(t *testing.T) {
	var gotAuth, gotBeta, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		gotUA = r.Header.Get("User-Agent")
		w.Write([]byte(sample))
	}))
	defer srv.Close()

	u, err := usage.FetchAt(context.Background(), srv.Client(), srv.URL, "tok-123", "9.9.9")
	require.NoError(t, err)

	require.Equal(t, "Bearer tok-123", gotAuth)
	require.Equal(t, "oauth-2025-04-20", gotBeta)
	require.Equal(t, "claude-code/9.9.9", gotUA, "User-Agent must carry the claude-code/<version> tag to avoid the throttled bucket")

	require.Equal(t, 8.0, u.FiveHour.Utilization)
	require.Equal(t, 17.0, u.SevenDay.Utilization)
	require.False(t, u.FiveHour.ResetsAt.IsZero())
	require.Len(t, u.Limits, 3)
	require.True(t, u.Limits[1].IsActive)
	require.True(t, u.Limits[2].ResetsAt.IsZero(), "a null resets_at must decode to the zero time")
}

func TestFetchDefaultVersion(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Write([]byte(`{"five_hour":{"utilization":0},"seven_day":{"utilization":0}}`))
	}))
	defer srv.Close()

	_, err := usage.FetchAt(context.Background(), srv.Client(), srv.URL, "tok", "")
	require.NoError(t, err)
	require.Equal(t, "claude-code/"+usage.FallbackVersion, gotUA)
}

func TestFetchStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"rate_limit_error"}`))
	}))
	defer srv.Close()

	_, err := usage.FetchAt(context.Background(), srv.Client(), srv.URL, "tok", "1.0.0")
	require.Error(t, err)

	var se *usage.StatusError
	require.True(t, errors.As(err, &se), "a non-200 must surface as *StatusError")
	require.Equal(t, http.StatusTooManyRequests, se.Code)
}
