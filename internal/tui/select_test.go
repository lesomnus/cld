package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/require"
)

// key builds the KeyMsg an Update sees for a named key like "down" or a rune.
func key(s string) tea.KeyMsg {
	switch s {
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// drive replays a sequence of keys through a select model and returns the final
// state.
func drive(m selectModel, keys ...string) selectModel {
	for _, k := range keys {
		next, _ := m.Update(key(k))
		m = next.(selectModel)
	}
	return m
}

func TestSelectModel(t *testing.T) {
	items := []Choice{{Title: "a"}, {Title: "b"}, {Title: "c"}}

	t.Run("enter chooses the cursor row", func(t *testing.T) {
		m := drive(newSelectModel("pick", items), "down", "down", "enter")
		require.Equal(t, 2, m.chosen)
		require.False(t, m.abort)
	})
	t.Run("cursor stops at the top", func(t *testing.T) {
		m := drive(newSelectModel("pick", items), "up", "up", "enter")
		require.Equal(t, 0, m.chosen)
	})
	t.Run("cursor stops at the bottom", func(t *testing.T) {
		m := drive(newSelectModel("pick", items), "down", "down", "down", "down", "enter")
		require.Equal(t, 2, m.chosen)
	})
	t.Run("j and k move like arrows", func(t *testing.T) {
		m := drive(newSelectModel("pick", items), "j", "j", "k", "enter")
		require.Equal(t, 1, m.chosen)
	})
	t.Run("esc aborts without a choice", func(t *testing.T) {
		m := drive(newSelectModel("pick", items), "down", "esc")
		require.True(t, m.abort)
		require.Equal(t, -1, m.chosen)
	})
	t.Run("q aborts", func(t *testing.T) {
		m := drive(newSelectModel("pick", items), "q")
		require.True(t, m.abort)
	})
	t.Run("g/G jump to ends", func(t *testing.T) {
		m := drive(newSelectModel("pick", items), "G", "enter")
		require.Equal(t, 2, m.chosen)
		m = drive(newSelectModel("pick", items), "G", "g", "enter")
		require.Equal(t, 0, m.chosen)
	})
	t.Run("view hides once resolved", func(t *testing.T) {
		m := drive(newSelectModel("pick", items), "enter")
		require.Empty(t, m.View())
	})
	t.Run("view shows the cursor while running", func(t *testing.T) {
		m := newSelectModel("pick", items)
		require.Contains(t, m.View(), "pick")
	})
}

func TestConfirmModel(t *testing.T) {
	t.Run("defaults to no on enter", func(t *testing.T) {
		next, _ := confirmModel{prompt: "ok?"}.Update(key("enter"))
		m := next.(confirmModel)
		require.True(t, m.done)
		require.False(t, m.yes)
	})
	t.Run("y answers yes", func(t *testing.T) {
		next, _ := confirmModel{prompt: "ok?"}.Update(key("y"))
		m := next.(confirmModel)
		require.True(t, m.done)
		require.True(t, m.yes)
	})
	t.Run("toggle then enter", func(t *testing.T) {
		m := confirmModel{prompt: "ok?"}
		next, _ := m.Update(key("left"))
		m = next.(confirmModel)
		next, _ = m.Update(key("enter"))
		m = next.(confirmModel)
		require.True(t, m.yes)
	})
	t.Run("esc aborts", func(t *testing.T) {
		next, _ := confirmModel{prompt: "ok?"}.Update(key("esc"))
		require.True(t, next.(confirmModel).abort)
	})
}
