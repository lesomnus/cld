package broker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/cld/internal/broker"
	"github.com/stretchr/testify/require"
)

// memStore is an in-memory Store for tests.
type memStore struct{ c *broker.Credentials }

func (m *memStore) Load() (*broker.Credentials, error) {
	if m.c == nil {
		return &broker.Credentials{}, nil
	}
	cp := *m.c
	return &cp, nil
}
func (m *memStore) Save(c *broker.Credentials) error { cp := *c; m.c = &cp; return nil }

// fakeTokenEndpoint mocks claude.ai/v1/oauth/token: it rotates the refresh token
// on each call and reports how many times it was hit.
func fakeTokenEndpoint(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	var n int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			GrantType    string `json:"grant_type"`
			RefreshToken string `json:"refresh_token"`
			ClientID     string `json:"client_id"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Equal(t, "refresh_token", in.GrantType)
		require.NotEmpty(t, in.RefreshToken)
		require.NotEmpty(t, in.ClientID)
		i := atomic.AddInt32(&n, 1)
		if hits != nil {
			atomic.StoreInt32(hits, i)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-v" + strconv.Itoa(int(i)),
			"refresh_token": "refresh-v" + strconv.Itoa(int(i)),
			"expires_in":    28800,
		})
	}))
}

func TestTokenRefreshesAndRotates(t *testing.T) {
	var hits int32
	ep := fakeTokenEndpoint(t, &hits)
	defer ep.Close()

	store := &memStore{c: &broker.Credentials{
		RefreshToken: "refresh-v0",
		// already expired -> forces a refresh
		AccessToken: "stale",
		ExpiresAt:   time.Unix(0, 0),
	}}
	now := time.Unix(1000, 0)
	b := broker.New(store, broker.WithTokenURL(ep.URL), broker.WithClock(func() time.Time { return now }))

	tok, err := b.Token(context.Background())
	require.NoError(t, err)
	require.Equal(t, "access-v1", tok)
	require.Equal(t, int32(1), atomic.LoadInt32(&hits), "one refresh")

	// Rotation persisted: the store now holds the rotated refresh token and the
	// new expiry (now + expires_in).
	require.Equal(t, "refresh-v1", store.c.RefreshToken)
	require.Equal(t, now.Add(28800*time.Second), store.c.ExpiresAt)

	// A second call within validity reuses the cached token — no new refresh.
	tok, err = b.Token(context.Background())
	require.NoError(t, err)
	require.Equal(t, "access-v1", tok)
	require.Equal(t, int32(1), atomic.LoadInt32(&hits), "no extra refresh while valid")
}

func TestTokenErrorsWithoutRefreshToken(t *testing.T) {
	b := broker.New(&memStore{}) // empty store, no refresh token
	_, err := b.Token(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "no refresh token")
}

func TestHandlerInjectsBearerAndForwards(t *testing.T) {
	var hits int32
	ep := fakeTokenEndpoint(t, &hits)
	defer ep.Close()

	// upstream echoes what auth headers it actually received.
	var gotAuth, gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("PONG"))
	}))
	defer upstream.Close()

	store := &memStore{c: &broker.Credentials{RefreshToken: "refresh-v0", ExpiresAt: time.Unix(0, 0)}}
	now := time.Unix(1000, 0)
	b := broker.New(store,
		broker.WithTokenURL(ep.URL),
		broker.WithUpstreamURL(upstream.URL),
		broker.WithClock(func() time.Time { return now }),
	)

	px := httptest.NewServer(b.Handler())
	defer px.Close()

	// The session sends a placeholder bearer + a stray x-api-key; the proxy must
	// replace the bearer with the real token and drop the x-api-key.
	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer placeholder")
	req.Header.Set("X-Api-Key", "should-be-dropped")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()

	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "Bearer access-v1", gotAuth, "proxy injected the real token")
	require.Empty(t, gotAPIKey, "proxy dropped the session's x-api-key")
}
