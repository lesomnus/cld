package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAbbreviateHomeIn(t *testing.T) {
	const home = "/home/me"
	cases := []struct {
		p, home, want string
	}{
		{"/home/me/src/app", home, "~/src/app"},
		{"/home/me", home, "~"},
		{"/home/me/.cld", home, "~/.cld"},
		{"/home/other/x", home, "/home/other/x"},   // different user, untouched
		{"/home/me2/x", home, "/home/me2/x"},        // prefix look-alike, not fooled
		{"/var/lib", home, "/var/lib"},              // outside home
		{"", home, ""},                              // empty path
		{"/home/me/src", "", "/home/me/src"},        // no home known
		{"/anything", "/", "/anything"},             // pathological home "/"
	}
	for _, c := range cases {
		require.Equalf(t, c.want, abbreviate_home_in(c.p, c.home), "abbreviate_home_in(%q, %q)", c.p, c.home)
	}
}

func TestLsFitColumns(t *testing.T) {
	rows := [][]string{
		{"web", "w", "abc123", "running", "1.2.3", "~/src/web"},
	}
	full := lsTableWidth(rows, []int{0, 1, 2, 3, 4, 5})

	cases := []struct {
		name  string
		width int
		want  []int
	}{
		{"wide enough shows all", full, []int{0, 1, 2, 3, 4, 5}},
		{"unlimited shows all", 1 << 30, []int{0, 1, 2, 3, 4, 5}},
		{"drop version first", full - 1, []int{0, 1, 2, 3, 5}},
		{"very narrow keeps name only", 1, []int{0}},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, lsFitColumns(rows, c.width), "%s", c.name)
	}
}

func TestLsDropOrderReverseImportance(t *testing.T) {
	// Every non-NAME column is droppable exactly once and NAME never is, so a
	// vanishing width peels columns off in strict reverse-importance order.
	rows := [][]string{{"web", "w", "abc123", "running", "1.2.3", "~/src/web"}}
	prev := lsFitColumns(rows, 1<<30)
	for w := lsTableWidth(rows, prev); w >= 0; w-- {
		cols := lsFitColumns(rows, w)
		require.Subsetf(t, prev, cols, "columns only shrink as width drops (at %d)", w)
		require.Containsf(t, cols, 0, "NAME is never dropped (at %d)", w)
		prev = cols
	}
	require.Equal(t, []int{0}, prev)
}
