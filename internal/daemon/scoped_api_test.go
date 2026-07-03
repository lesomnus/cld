package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestScopedAPI pins the security property of the in-container relay: a
// container reaches only its own session, never another project's.
func TestScopedAPI(t *testing.T) {
	d := &Daemon{entries: map[string]*entry{}}
	mk := func(id, name string) {
		e := &entry{id: id}
		e.item = Item{ID: id, Name: name}
		e.publish()
		d.entries[id] = e
	}
	mk("idA", "alpha")
	mk("idB", "bravo")

	h := d.scoped_api("idA") // the relay serving container idA ("alpha")

	t.Run("items are filtered to the caller's own session", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/items", nil))
		require.Equal(t, http.StatusOK, rr.Code)
		require.Contains(t, rr.Body.String(), "alpha")
		require.NotContains(t, rr.Body.String(), "bravo")
	})

	t.Run("cannot address another container's session", func(t *testing.T) {
		cases := []struct{ method, path string }{
			{http.MethodGet, "/session/attach?name=bravo"},
			{http.MethodPost, "/session/new?name=bravo"},
			{http.MethodPost, "/down?name=bravo"},
		}
		for _, c := range cases {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(c.method, c.path, nil))
			require.Equal(t, http.StatusForbidden, rr.Code, c.path)
		}
	})

	t.Run("its own session passes the guard", func(t *testing.T) {
		// attach for "alpha" passes only_self, then stops at 501 (this daemon is
		// not containerized) — the point is it is NOT forbidden.
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/session/attach?name=alpha", nil))
		require.NotEqual(t, http.StatusForbidden, rr.Code)
	})

	t.Run("notify is scoped to the caller's own container id", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/notify/exited?container=idB&code=1", nil))
		require.Equal(t, http.StatusForbidden, rr.Code)
	})
}
