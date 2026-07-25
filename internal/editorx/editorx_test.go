package editorx

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolve(t *testing.T) {
	t.Run("VISUAL wins over EDITOR", func(t *testing.T) {
		t.Setenv("VISUAL", "code --wait")
		t.Setenv("EDITOR", "vim")
		require.Equal(t, []string{"code", "--wait"}, Resolve())
	})
	t.Run("EDITOR when VISUAL is unset", func(t *testing.T) {
		t.Setenv("VISUAL", "")
		t.Setenv("EDITOR", "nano")
		require.Equal(t, []string{"nano"}, Resolve())
	})
	t.Run("falls back to a known editor when neither is set", func(t *testing.T) {
		t.Setenv("VISUAL", "")
		t.Setenv("EDITOR", "")
		got := Resolve()
		require.NotEmpty(t, got)
		require.Contains(t, fallbacks, got[0])
	})
}
