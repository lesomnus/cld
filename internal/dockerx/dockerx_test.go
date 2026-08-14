package dockerx

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A directory needs execute wherever it grants read, or its contents are
// unreachable — a 0600 tree would otherwise be copied in unusable.
func TestDirModeFor(t *testing.T) {
	for _, tc := range []struct{ mode, want int64 }{
		{0o600, 0o700},
		{0o640, 0o750},
		{0o644, 0o755},
		{0o400, 0o500},
		{0o000, 0o000},
	} {
		require.Equal(t, tc.want, dirModeFor(tc.mode),
			"mode %o", tc.mode)
	}
}
