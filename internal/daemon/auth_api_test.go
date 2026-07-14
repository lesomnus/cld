package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHandleSetCredentials(t *testing.T) {
	const login = `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":0}}`

	t.Run("stores a broker login from the body", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		rr := httptest.NewRecorder()
		d.handle_set_credentials(rr, httptest.NewRequest(http.MethodPost, "/auth/credentials",
			strings.NewReader(login)))
		require.Equal(t, http.StatusNoContent, rr.Code)
		require.True(t, d.broker.HasCredentials())
	})

	t.Run("rejects a body without a claudeAiOauth object", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		rr := httptest.NewRecorder()
		d.handle_set_credentials(rr, httptest.NewRequest(http.MethodPost, "/auth/credentials",
			strings.NewReader(`{}`)))
		require.Equal(t, http.StatusBadRequest, rr.Code)
		require.False(t, d.broker.HasCredentials())
	})

	t.Run("rejects a login without a refresh token", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		rr := httptest.NewRecorder()
		d.handle_set_credentials(rr, httptest.NewRequest(http.MethodPost, "/auth/credentials",
			strings.NewReader(`{"claudeAiOauth":{"accessToken":"a"}}`)))
		require.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("reachable through the in-container scoped relay", func(t *testing.T) {
		d, _ := newTestDaemon(t)
		d.entries = map[string]*entry{}
		h := d.scoped_api("idA")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/auth/credentials", strings.NewReader(login)))
		require.Equal(t, http.StatusNoContent, rr.Code)
		require.True(t, d.broker.HasCredentials())
	})
}
