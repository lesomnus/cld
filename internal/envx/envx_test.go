package envx_test

import (
	"testing"

	"github.com/lesomnus/cld/internal/envx"
	"github.com/stretchr/testify/require"
)

func ptr(s string) *string { return &s }

// vars maps a resolved result to key -> value for terse assertions; an unset
// variable is absent, exactly as it is inside the container.
func vars(r envx.Result) map[string]string {
	out := map[string]string{}
	for _, v := range r.Vars {
		if !v.Unset {
			out[v.Key] = v.Value
		}
	}
	return out
}

func origins(r envx.Result) map[string]string {
	out := map[string]string{}
	for _, v := range r.Vars {
		out[v.Key] = v.Origin
	}
	return out
}

func TestResolveLayers(t *testing.T) {
	t.Run("a later layer wins", func(t *testing.T) {
		r := envx.Resolve([]string{"FOO=image"}, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{"FOO": ptr("a")}},
			envx.Layer{Origin: "b", Vars: map[string]*string{"FOO": ptr("b")}},
		)
		require.Equal(t, "b", vars(r)["FOO"])
		require.Equal(t, "b", origins(r)["FOO"])
	})

	t.Run("untouched container variables are reported but not re-sent", func(t *testing.T) {
		r := envx.Resolve([]string{"PATH=/usr/bin", "FOO=image"}, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{"FOO": ptr("a")}},
		)
		require.Equal(t, "/usr/bin", vars(r)["PATH"])
		require.Equal(t, envx.OriginContainer, origins(r)["PATH"])
		require.Equal(t, []string{"FOO=a"}, r.Overrides())
	})

	t.Run("malformed container entries are skipped", func(t *testing.T) {
		r := envx.Resolve([]string{"NOPE", "=empty", "OK=1"}, nil)
		require.Equal(t, map[string]string{"OK": "1"}, vars(r))
	})

	t.Run("result is ordered by key", func(t *testing.T) {
		r := envx.Resolve([]string{"C=3", "A=1"}, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{"B": ptr("2")}},
		)
		require.Equal(t, []string{"A", "B", "C"}, []string{r.Vars[0].Key, r.Vars[1].Key, r.Vars[2].Key})
	})
}

func TestResolveExpand(t *testing.T) {
	t.Run("extends a container value", func(t *testing.T) {
		r := envx.Resolve([]string{"PATH=/usr/bin"}, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{"PATH": ptr("${PATH}:/opt/bin")}},
		)
		require.Equal(t, "/usr/bin:/opt/bin", vars(r)["PATH"])
	})

	t.Run("extends across layers", func(t *testing.T) {
		r := envx.Resolve([]string{"PATH=/usr/bin"}, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{"PATH": ptr("${PATH}:/a")}},
			envx.Layer{Origin: "b", Vars: map[string]*string{"PATH": ptr("${PATH}:/b")}},
		)
		require.Equal(t, "/usr/bin:/a:/b", vars(r)["PATH"])
	})

	t.Run("siblings in one layer cannot see each other", func(t *testing.T) {
		// Otherwise the result would depend on YAML map ordering.
		r := envx.Resolve(nil, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{
				"A": ptr("1"),
				"B": ptr("[${A}]"),
			}},
		)
		require.Equal(t, "[]", vars(r)["B"])
	})

	t.Run("fallback applies when unset or empty", func(t *testing.T) {
		r := envx.Resolve([]string{"EMPTY="}, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{
				"A": ptr("${MISSING:-fallback}"),
				"B": ptr("${EMPTY:-fallback}"),
			}},
		)
		require.Equal(t, "fallback", vars(r)["A"])
		require.Equal(t, "fallback", vars(r)["B"])
	})

	t.Run("fallback defers to an existing value", func(t *testing.T) {
		r := envx.Resolve([]string{"GOFLAGS=-race"}, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{"GOFLAGS": ptr("${GOFLAGS:--mod=mod}")}},
		)
		require.Equal(t, "-race", vars(r)["GOFLAGS"])
	})

	t.Run("namespaced references", func(t *testing.T) {
		extra := func(ns, name string) (string, bool) {
			if ns == "env" && name == "SECRET" {
				return "s3cret", true
			}
			return "", false
		}
		r := envx.Resolve([]string{"HOME=/home/dev"}, extra,
			envx.Layer{Origin: "a", Vars: map[string]*string{
				"A": ptr("${containerEnv:HOME}/x"),
				"B": ptr("${env:SECRET}"),
				"C": ptr("${localEnv:ANYTHING}"),
			}},
		)
		require.Equal(t, "/home/dev/x", vars(r)["A"])
		require.Equal(t, "s3cret", vars(r)["B"])
		require.Equal(t, "", vars(r)["C"])
	})
}

func TestResolveUnset(t *testing.T) {
	t.Run("removes an inherited variable", func(t *testing.T) {
		r := envx.Resolve([]string{"LESS=-R", "KEEP=1"}, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{"LESS": nil}},
		)
		require.Equal(t, []string{"LESS"}, r.Unset())
		require.NotContains(t, r.Overrides(), "LESS=")
		_, ok := r.Value("LESS")
		require.False(t, ok)
	})

	t.Run("an unset variable reads as absent", func(t *testing.T) {
		r := envx.Resolve([]string{"FOO=x"}, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{"FOO": nil}},
			envx.Layer{Origin: "b", Vars: map[string]*string{"BAR": ptr("${FOO:-gone}")}},
		)
		require.Equal(t, "gone", vars(r)["BAR"])
	})

	t.Run("a later layer can set it again", func(t *testing.T) {
		r := envx.Resolve([]string{"FOO=x"}, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{"FOO": nil}},
			envx.Layer{Origin: "b", Vars: map[string]*string{"FOO": ptr("back")}},
		)
		require.Empty(t, r.Unset())
		require.Equal(t, "back", vars(r)["FOO"])
	})

	t.Run("an empty value is not an unset", func(t *testing.T) {
		r := envx.Resolve([]string{"FOO=x"}, nil,
			envx.Layer{Origin: "a", Vars: map[string]*string{"FOO": ptr("")}},
		)
		require.Empty(t, r.Unset())
		require.Equal(t, []string{"FOO="}, r.Overrides())
	})
}

func TestExpand(t *testing.T) {
	look := func(ns, name string) (string, bool) {
		if ns == "" && name == "A" {
			return "1", true
		}
		return "", false
	}

	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", ""},
		{"plain", "plain"},
		{"${A}", "1"},
		{"x${A}y", "x1y"},
		{"${MISSING}", ""},
		{"${MISSING:-d}", "d"},
		{"${MISSING:-${A}}", "1"},            // nested fallback
		{"${MISSING:-a:b}", "a:b"},           // a colon inside a fallback
		{"$$", "$"},                          // literal
		{"$${A}", "${A}"},                    // escaped, not expanded
		{"p$ssw0rd", "p$ssw0rd"},             // a lone $ is literal
		{"end$", "end$"},                     // trailing $
		{"${unterminated", "${unterminated"}, // left as written
	} {
		require.Equal(t, tc.want, envx.Expand(tc.in, look), "input %q", tc.in)
	}
}

func TestValidKey(t *testing.T) {
	for _, k := range []string{"A", "_", "FOO_BAR", "a1", "_1"} {
		require.True(t, envx.ValidKey(k), k)
	}
	for _, k := range []string{"", "1A", "A-B", "A B", "A=B", "A.B", "FOO="} {
		require.False(t, envx.ValidKey(k), k)
	}
}
