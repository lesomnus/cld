package cmd

import (
	"io"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// TestPurgeFlagValidation covers the argument logic that guards the destructive
// paths before any daemon call, mirroring `cld down`: a name and --all are
// mutually exclusive, and a plain `cld purge` still needs a name.
func TestPurgeFlagValidation(t *testing.T) {
	run := func(args ...string) error {
		ctx := use_config.Into(t.Context(), &config.Config{})
		cmd := NewCmdPurge()
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
	t.Run("a bare purge needs a name", func(t *testing.T) {
		err := run()
		require.Error(t, err)
		require.Contains(t, err.Error(), "name required")
	})
}
