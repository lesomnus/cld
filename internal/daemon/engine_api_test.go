package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDockerEngineAPI covers what `cld docker` resolves before it runs
// anything: the engine is reachable only from the devcontainer's private
// network, so a client has to be told which container to drive.
func TestDockerEngineAPI(t *testing.T) {
	d, _ := newTestDaemon(t)

	e := &entry{id: "idA", mbox: new_mailbox()}
	e.item = Item{ID: "idA", Name: "alpha", LocalFolder: "/work/api"}
	e.publish()
	d.entries = map[string]*entry{"idA": e}

	// The lookup runs on the container's worker, as the other read endpoints do.
	go e.mbox.run()
	t.Cleanup(e.mbox.close)

	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		d.handle_get_engine(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}

	t.Run("requires a name", func(t *testing.T) {
		require.Equal(t, http.StatusBadRequest, get("/docker/engine").Code)
	})

	t.Run("unknown devcontainer", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, get("/docker/engine?name=nope").Code)
	})

	t.Run("says so when the project has no engine configured", func(t *testing.T) {
		// Rather than a bare 404, which reads as "cld is broken".
		rr := get("/docker/engine?name=alpha")
		require.Equal(t, http.StatusConflict, rr.Code)
		require.Contains(t, rr.Body.String(), "mode: dind")
	})

	// The success path needs a real engine; TestDindLifecycle covers it.
}

// The endpoint is host-only: driving the engine means exec'ing into its
// container, which needs the host's own engine — something a container cannot
// reach anyway.
func TestDockerEngineIsHostOnly(t *testing.T) {
	d := &Daemon{entries: map[string]*entry{}}
	e := &entry{id: "idA"}
	e.item = Item{ID: "idA", Name: "alpha"}
	e.publish()
	d.entries["idA"] = e

	rr := httptest.NewRecorder()
	d.scoped_api("idA").ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/docker/engine?name=alpha", nil))
	require.Equal(t, http.StatusNotFound, rr.Code, "the relay must not route it at all")
}

func TestDockerEngineJSON(t *testing.T) {
	// The client depends on these names.
	b, err := json.Marshal(DockerEngine{
		Container: "abc", Name: "cld-api-dind", Endpoint: "tcp://cld-api-dind:2375", Running: true,
	})
	require.NoError(t, err)
	require.JSONEq(t,
		`{"container":"abc","name":"cld-api-dind","endpoint":"tcp://cld-api-dind:2375","running":true}`,
		string(b))
}
