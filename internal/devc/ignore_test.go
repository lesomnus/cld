package devc_test

import (
	"testing"

	"github.com/lesomnus/cld/internal/devc"
	"github.com/stretchr/testify/require"
)

func TestMatchPath(t *testing.T) {
	t.Run("plain glob does not cross separators", func(t *testing.T) {
		require.True(t, devc.MatchPath("/work/*", "/work/foo"))
		require.False(t, devc.MatchPath("/work/*", "/work/foo/bar"))
	})
	t.Run("double star crosses separators", func(t *testing.T) {
		require.True(t, devc.MatchPath("/work/**", "/work/foo/bar"))
		require.True(t, devc.MatchPath("/work/**/baz", "/work/foo/bar/baz"))
	})
	t.Run("double star matches zero directories", func(t *testing.T) {
		require.True(t, devc.MatchPath("/work/**/baz", "/work/baz"))
	})
	t.Run("no match", func(t *testing.T) {
		require.False(t, devc.MatchPath("/other/**", "/work/foo"))
	})
}

func TestIgnored(t *testing.T) {
	t.Run("by label", func(t *testing.T) {
		labels := map[string]string{devc.LabelIgnore: "true"}
		require.True(t, devc.Ignored(labels, "/work/foo", nil))
	})
	t.Run("label explicitly false", func(t *testing.T) {
		labels := map[string]string{devc.LabelIgnore: "false"}
		require.False(t, devc.Ignored(labels, "/work/foo", nil))
	})
	t.Run("by glob", func(t *testing.T) {
		require.True(t, devc.Ignored(nil, "/work/vendor/foo", []string{"/work/vendor/**"}))
		require.False(t, devc.Ignored(nil, "/work/mine", []string{"/work/vendor/**"}))
	})
}
