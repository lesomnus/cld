package daemon

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

func newAuthDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	d := &Daemon{
		cfg: &config.Config{DataDir: dir},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return d, dir
}

func TestHandleSetToken(t *testing.T) {
	t.Run("stores a token from the body with mode 0600", func(t *testing.T) {
		d, _ := newAuthDaemon(t)
		rr := httptest.NewRecorder()
		d.handle_set_token(rr, httptest.NewRequest(http.MethodPost, "/auth/token",
			strings.NewReader("sk-ant-oat01-abc\n"))) // trailing newline trimmed
		require.Equal(t, http.StatusNoContent, rr.Code)

		p := d.cfg.OAuthTokenStorePath()
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		require.Equal(t, "sk-ant-oat01-abc", string(b))

		info, err := os.Stat(p)
		require.NoError(t, err)
		require.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
	})

	t.Run("rejects an empty token", func(t *testing.T) {
		d, dir := newAuthDaemon(t)
		rr := httptest.NewRecorder()
		d.handle_set_token(rr, httptest.NewRequest(http.MethodPost, "/auth/token",
			strings.NewReader("   \n")))
		require.Equal(t, http.StatusBadRequest, rr.Code)
		_, err := os.Stat(dir + "/oauth-token")
		require.ErrorIs(t, err, fs.ErrNotExist) // nothing written
	})

	t.Run("rejects a token with embedded whitespace", func(t *testing.T) {
		d, _ := newAuthDaemon(t)
		rr := httptest.NewRecorder()
		d.handle_set_token(rr, httptest.NewRequest(http.MethodPost, "/auth/token",
			strings.NewReader("tok en")))
		require.Equal(t, http.StatusBadRequest, rr.Code)
	})

	t.Run("reachable through the in-container scoped relay", func(t *testing.T) {
		d, _ := newAuthDaemon(t)
		d.entries = map[string]*entry{}
		h := d.scoped_api("idA")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/auth/token",
			strings.NewReader("sk-ant-oat01-xyz")))
		require.Equal(t, http.StatusNoContent, rr.Code)
		b, err := os.ReadFile(d.cfg.OAuthTokenStorePath())
		require.NoError(t, err)
		require.Equal(t, "sk-ant-oat01-xyz", string(b))
	})
}

func TestOAuthTokenFilePrecedence(t *testing.T) {
	d, _ := newAuthDaemon(t)
	d.cfg.Auth.OAuthTokenFile = "/configured/path"

	// With no stored token, the configured path is used.
	require.Equal(t, "/configured/path", d.oauth_token_file())

	// A token set via the API takes precedence over the configured path.
	require.NoError(t, os.WriteFile(d.cfg.OAuthTokenStorePath(), []byte("sk-ant-oat01-abc"), 0o600))
	require.Equal(t, d.cfg.OAuthTokenStorePath(), d.oauth_token_file())
}
