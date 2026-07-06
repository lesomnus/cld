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
