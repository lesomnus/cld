package cmd

import (
	"io"
	"strings"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// TestConfirmed pins the consent gate for the destructive `cld down --all`:
// only an explicit y/yes proceeds; a bare Enter, EOF (non-interactive stdin),
// or anything else reads as "no".
func TestConfirmed(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"  y  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false}, // bare Enter
		{"", false},   // EOF / piped, non-interactive
		{"maybe\n", false},
		{"yy\n", false},
		{"ya\n", false},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, confirmed(strings.NewReader(c.in)), "input %q", c.in)
	}
}

// TestDownFlagValidation covers the argument logic that guards the destructive
// paths before any daemon call: a name and --all are mutually exclusive, and a
// plain `cld down` still needs a name.
func TestDownFlagValidation(t *testing.T) {
	run := func(args ...string) error {
		ctx := use_config.Into(t.Context(), &config.Config{})
		cmd := NewCmdDown()
		cmd.Writer = io.Discard
		cmd.ErrWriter = io.Discard
		return cmd.Run(ctx, args)
	}

	t.Run("name and --all are mutually exclusive", func(t *testing.T) {
		// xli requires flags before positional args, so the reachable
		// contradictory form is `--all <name>`.
		err := run("--all", "myapp")
		require.Error(t, err)
		require.Contains(t, err.Error(), "not both")
	})
	t.Run("a bare down needs a name", func(t *testing.T) {
		err := run()
		require.Error(t, err)
		require.Contains(t, err.Error(), "name required")
	})
}
