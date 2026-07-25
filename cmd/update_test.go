package cmd

import (
	"io"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// TestUpdateFlagValidation covers the argument logic that guards before any
// daemon call: a name and --all are mutually exclusive. Unlike `cld down`, a
// bare `cld update` is valid (it targets the sole/own devcontainer), so that
// path is not asserted here — it reaches the daemon.
func TestUpdateFlagValidation(t *testing.T) {
	run := func(args ...string) error {
		ctx := use_config.Into(t.Context(), &config.Config{})
		cmd := NewCmdUpdate()
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
	t.Run("an unknown channel is rejected before any daemon call", func(t *testing.T) {
		err := run("--channel", "beta", "myapp")
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown channel")
	})
}

// TestValidateChannel pins the accepted --channel values: only the two the
// release server serves, plus empty (use the daemon's configured channel).
func TestValidateChannel(t *testing.T) {
	require.NoError(t, validate_channel(""))
	require.NoError(t, validate_channel("stable"))
	require.NoError(t, validate_channel("latest"))
	require.Error(t, validate_channel("beta"))
	require.Error(t, validate_channel("Latest"))
}
