package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionStore(t *testing.T) {
	s := &sessionStore{dir: t.TempDir()}

	t.Run("missing record is zero", func(t *testing.T) {
		st := s.get("nope")
		require.Equal(t, "", st.Gen)
		require.False(t, st.Ended)
	})
	t.Run("set and get", func(t *testing.T) {
		require.NoError(t, s.set("abc", sessionState{Gen: "2026-01-01T00:00:00Z", Ended: true}))
		st := s.get("abc")
		require.Equal(t, "2026-01-01T00:00:00Z", st.Gen)
		require.True(t, st.Ended)
	})
	t.Run("clear", func(t *testing.T) {
		require.NoError(t, s.set("xyz", sessionState{Gen: "g", Ended: true}))
		s.clear("xyz")
		require.False(t, s.get("xyz").Ended)
	})
}
