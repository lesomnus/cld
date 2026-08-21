package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// `cld config --daemon` exists because a client and the daemon read different
// files on different filesystems: when a setting "did not apply", the only
// honest answer is what the daemon itself loaded.
func TestDaemonConfigAPI(t *testing.T) {
	d, cfg := newTestDaemon(t)
	cfg.Files = []config.FileSpec{{Src: "~/.config/gh/hosts.yml", Dst: "${HOME}/.config/gh/hosts.yml"}}

	rr := httptest.NewRecorder()
	d.handle_get_config_file(rr, httptest.NewRequest(http.MethodGet, "/config", nil))
	require.Equal(t, http.StatusOK, rr.Code)

	var got DaemonConfig
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))

	// The settings in question have to be visible, not summarized away.
	require.Contains(t, got.YAML, "hosts.yml")
	require.Contains(t, got.YAML, "files:")

	t.Run("says where it came from", func(t *testing.T) {
		// Empty here (built in code); a real daemon reports the loaded path, and
		// its emptiness is itself the answer when a file was expected.
		require.Equal(t, cfg.Path(), got.Path)
	})
}

// The config is global — every project's settings, and any secret written into
// them — so a container must not be able to read it through the relay.
func TestDaemonConfigIsHostOnly(t *testing.T) {
	d, _ := newTestDaemon(t)
	e := &entry{id: "idA"}
	e.item = Item{ID: "idA", Name: "alpha"}
	e.publish()
	d.entries = map[string]*entry{"idA": e}

	rr := httptest.NewRecorder()
	d.scoped_api("idA").ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/config", nil))
	require.Equal(t, http.StatusNotFound, rr.Code, "the relay must not route it at all")
}
