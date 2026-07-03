package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStripCredentialHelpers(t *testing.T) {
	t.Run("drops helpers, keeps identity and other keys", func(t *testing.T) {
		in := "[user]\n\tname = Alice\n\temail = alice@example.com\n" +
			"[credential]\n\thelper = gopass\n\tuseHttpPath = true\n" +
			"[credential \"https://github.com\"]\n\thelper = !gh auth git-credential\n" +
			"[core]\n\teditor = vim\n"

		out := string(strip_credential_helpers([]byte(in)))

		require.NotContains(t, out, "gopass")
		require.NotContains(t, out, "!gh auth")
		require.Contains(t, out, "name = Alice")
		require.Contains(t, out, "email = alice@example.com")
		require.Contains(t, out, "useHttpPath = true") // non-helper credential key stays
		require.Contains(t, out, "editor = vim")
		require.Contains(t, out, "[credential]") // sections themselves remain
	})

	t.Run("case-insensitive section and key", func(t *testing.T) {
		in := "[CREDENTIAL]\n\tHelper = gopass\n"
		require.NotContains(t, string(strip_credential_helpers([]byte(in))), "gopass")
	})

	t.Run("only within credential sections", func(t *testing.T) {
		// A git alias literally named "helper" must survive.
		in := "[alias]\n\thelper = status\n[credential]\n\thelper = gopass\n"
		out := string(strip_credential_helpers([]byte(in)))
		require.Contains(t, out, "helper = status")
		require.NotContains(t, out, "gopass")
	})
}
