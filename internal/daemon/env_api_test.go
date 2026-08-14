package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// TestSessionEnvAPI covers what `cld setting env` shows: the effective
// environment with the layer that decided each variable, which is the only way
// a user can tell why something they set did not take.
func TestSessionEnvAPI(t *testing.T) {
	d, cfg := newTestDaemon(t)
	cfg.Env = config.EnvMap{"EDITOR": strp("vim"), "LESS": nil}

	e := &entry{
		id:            "idA",
		mbox:          new_mailbox(),
		cfg_dir:       "/home/dev/.claude",
		container_env: []string{"PATH=/usr/bin", "LESS=-R"},
	}
	e.item = Item{ID: "idA", Name: "alpha"}
	e.publish()
	d.entries = map[string]*entry{"idA": e}

	// The handler hands work to the container's worker, as the other read
	// endpoints do, so one has to be running.
	go e.mbox.run()
	t.Cleanup(e.mbox.close)

	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		d.handle_get_env(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}

	t.Run("reports each variable with its origin", func(t *testing.T) {
		rr := get("/session/env?name=alpha")
		require.Equal(t, http.StatusOK, rr.Code)

		var out struct {
			Vars []EnvVar `json:"vars"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))

		by := map[string]EnvVar{}
		for _, v := range out.Vars {
			by[v.Key] = v
		}

		require.Equal(t, "vim", by["EDITOR"].Value)
		require.Equal(t, envOriginConfig, by["EDITOR"].Origin)

		// Inherited, and reported as such rather than left out.
		require.Equal(t, "/usr/bin", by["PATH"].Value)
		require.Equal(t, "container", by["PATH"].Origin)

		// A removal is visible as a removal, not as an absence.
		require.True(t, by["LESS"].Unset)
		require.Equal(t, envOriginConfig, by["LESS"].Origin)

		require.Equal(t, "/home/dev/.claude", by["CLAUDE_CONFIG_DIR"].Value)
		require.Equal(t, envOriginManaged, by["CLAUDE_CONFIG_DIR"].Origin)
	})

	t.Run("requires a name", func(t *testing.T) {
		require.Equal(t, http.StatusBadRequest, get("/session/env").Code)
	})

	t.Run("unknown devcontainer", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, get("/session/env?name=nope").Code)
	})

	t.Run("not provisioned yet", func(t *testing.T) {
		bare := &entry{id: "idB", mbox: new_mailbox()}
		bare.item = Item{ID: "idB", Name: "bravo"}
		bare.publish()
		d.entries["idB"] = bare
		go bare.mbox.run()
		t.Cleanup(bare.mbox.close)

		require.Equal(t, http.StatusConflict, get("/session/env?name=bravo").Code)
	})
}
