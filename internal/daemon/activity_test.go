package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyActivity(t *testing.T) {
	cases := []struct {
		name  string
		pane  string
		title string
		want  Activity
	}{
		{"working shows interrupt hint", "✳ Herding… (esc to interrupt)", "Some title", ActivityWorking},
		{"working outranks a missing title", "esc to interrupt", "", ActivityWorking},
		{"idle prompt with no conversation", "│ > try \"fix the bug\"", "", ActivityIdle},
		{"waiting once a title exists", "│ > ", "Refactor the auth broker", ActivityWaiting},
		{"empty pane with a title is waiting", "", "Refactor the auth broker", ActivityWaiting},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, classifyActivity(c.pane, c.title), "%s", c.name)
	}
}
