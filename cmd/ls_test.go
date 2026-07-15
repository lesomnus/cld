package cmd

import (
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

func TestCardIdentityOmitsEmptyFields(t *testing.T) {
	// A missing alias must not leave a dangling " · " separator.
	line := cardIdentity(daemon.Item{Name: "web", ID: "abc123", Version: "1.2.3", LocalFolder: "~/src/web"})
	require.NotContains(t, line, " ·  · ")
	require.Contains(t, line, "abc123")
	require.NotContains(t, line, "· abc123 · ") // no leading empty alias before the id

	// A present alias is included, in order, before the container id.
	line = cardIdentity(daemon.Item{Name: "web", Alias: "w", ID: "abc123"})
	require.Contains(t, line, "w · abc123")
}
