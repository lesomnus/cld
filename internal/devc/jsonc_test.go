package devc_test

import (
	"encoding/json"
	"testing"

	"github.com/lesomnus/cld/internal/devc"
	"github.com/stretchr/testify/require"
)

func TestStripJSONC(t *testing.T) {
	parse := func(t *testing.T, src string) map[string]any {
		var v map[string]any
		err := json.Unmarshal(devc.StripJSONC([]byte(src)), &v)
		require.NoError(t, err)
		return v
	}

	t.Run("line comments", func(t *testing.T) {
		v := parse(t, "{\n// comment\n\"a\": 1\n}")
		require.Equal(t, float64(1), v["a"])
	})
	t.Run("block comments", func(t *testing.T) {
		v := parse(t, `{"a": /* nope */ 2}`)
		require.Equal(t, float64(2), v["a"])
	})
	t.Run("trailing commas", func(t *testing.T) {
		v := parse(t, "{\"a\": [1, 2,],\n\"b\": {\"c\": 3,},\n}")
		require.Len(t, v["a"], 2)
	})
	t.Run("trailing comma before comment", func(t *testing.T) {
		v := parse(t, "{\"a\": 1, // done\n}")
		require.Equal(t, float64(1), v["a"])
	})
	t.Run("strings are untouched", func(t *testing.T) {
		v := parse(t, `{"url": "http://x/y", "q": "a, // not a comment", "e": "\" /*"}`)
		require.Equal(t, "http://x/y", v["url"])
		require.Equal(t, "a, // not a comment", v["q"])
		require.Equal(t, `" /*`, v["e"])
	})
}
