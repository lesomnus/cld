package cmd

import (
	"io"
	"testing"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

// TestCatFileValidation covers the argument logic that runs before any daemon
// call: an unknown config file is rejected up front. The file is the second
// positional (after the name), so a name is supplied to reach it. It runs
// through the `setting` parent to also pin the `setting cat` routing.
func TestCatFileValidation(t *testing.T) {
	ctx := use_config.Into(t.Context(), &config.Config{})
	cmd := NewCmdSetting()
	cmd.Writer = io.Discard
	cmd.ErrWriter = io.Discard

	err := cmd.Run(ctx, []string{"cat", "myapp", "bogus"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown file")
}
