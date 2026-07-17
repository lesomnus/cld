package cmd

import (
	"strings"
	"testing"

	"github.com/lesomnus/cld/internal/daemon"
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
		{"/home/other/x", home, "/home/other/x"}, // different user, untouched
		{"/home/me2/x", home, "/home/me2/x"},     // prefix look-alike, not fooled
		{"/var/lib", home, "/var/lib"},           // outside home
		{"", home, ""},                           // empty path
		{"/home/me/src", "", "/home/me/src"},     // no home known
		{"/anything", "/", "/anything"},          // pathological home "/"
	}
	for _, c := range cases {
		require.Equalf(t, c.want, abbreviate_home_in(c.p, c.home), "abbreviate_home_in(%q, %q)", c.p, c.home)
	}
}

// TestCardsFixedWidthColumns checks the first-line identity fields line up in
// fixed columns even when names differ in length: the container id (and every
// field after it) must start at the same screen column on every card. Tests do
// not run on a TTY, so lipgloss renders without color and the padding is
// plain spaces we can measure directly.
func TestCardsFixedWidthColumns(t *testing.T) {
	var b strings.Builder
	err := renderLsCards(&b, []daemon.Item{
		{Name: "web", Alias: "w", ID: "aaaaaa", Status: daemon.StatusReady},
		{Name: "a-very-long-name", Alias: "svc", ID: "bbbbbb", Status: daemon.StatusReady},
	})
	require.NoError(t, err)

	// The "╭" line of each card carries the identity columns. Alias leads, so it
	// precedes the name; the container id column must start at the same screen
	// column on both cards despite the very different name lengths.
	lines := strings.Split(b.String(), "\n")
	l1, l2 := lines[0], lines[2]
	require.Equal(t, strings.Index(l1, "aaaaaa"), strings.Index(l2, "bbbbbb"), "container id column must align")
	require.Less(t, strings.Index(l1, "w"), strings.Index(l1, "web"), "alias precedes the full name")
}
