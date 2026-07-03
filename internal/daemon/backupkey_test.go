package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBackupKey(t *testing.T) {
	d := &Daemon{}

	t.Run("keyed by the devcontainer name, namespaced", func(t *testing.T) {
		e := &entry{dev_name: "lesomnus/cld"}
		e.item.LocalFolder = "/home/me/projects/cld"
		require.Equal(t, "cld-lesomnus-cld", d.backup_key(e))
	})
	t.Run("name is portable across paths", func(t *testing.T) {
		a := &entry{dev_name: "myapp"}
		a.item.LocalFolder = "/home/me/a/myapp"
		b := &entry{dev_name: "myapp"}
		b.item.LocalFolder = "/somewhere/else/myapp"
		require.Equal(t, d.backup_key(a), d.backup_key(b))
	})
	t.Run("no name falls back to folder plus a unique hash", func(t *testing.T) {
		a := &entry{}
		a.item.LocalFolder = "/home/me/a/work"
		b := &entry{}
		b.item.LocalFolder = "/home/me/b/work"

		ka, kb := d.backup_key(a), d.backup_key(b)
		require.True(t, strings.HasPrefix(ka, "cld-work-"))
		require.True(t, strings.HasPrefix(kb, "cld-work-"))
		require.NotEqual(t, ka, kb, "same basename at different paths must not collide")
	})
}
