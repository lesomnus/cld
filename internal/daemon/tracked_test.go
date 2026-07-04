package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsTracked pins the guard down --all relies on: a stale entry pointer,
// dropped by remove (as ensure does for an ignored container), is no longer
// considered tracked even after a fresh entry for the same id is created. This
// is what stops a queued down from acting on a container cld stopped managing.
func TestIsTracked(t *testing.T) {
	d := &Daemon{entries: map[string]*entry{}}

	e := d.get_or_create("abc")
	require.True(t, d.is_tracked(e))

	// remove drops it (and closes its worker) — the ignore path in ensure.
	d.remove(e)
	require.False(t, d.is_tracked(e))

	// A new entry for the same id is a distinct pointer; the stale one stays
	// untracked, so a down still holding it must not fire.
	e2 := d.get_or_create("abc")
	require.True(t, d.is_tracked(e2))
	require.False(t, d.is_tracked(e), "stale pointer must not be considered tracked")

	d.remove(e2)
}
