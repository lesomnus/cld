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

func TestFormatTokens(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{938, "938"},
		{999, "999"},
		{1_000, "1.0k"},
		{12_345, "12.3k"},
		{999_999, "1000.0k"},
		{1_000_000, "1.0M"},
		{1_250_000, "1.2M"}, // fmt rounds half to even
		{1_260_000, "1.3M"},
		{45_600_000, "45.6M"},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, formatTokens(c.n), "formatTokens(%d)", c.n)
	}
}

func TestFormatCost(t *testing.T) {
	require.Equal(t, "$0.00", formatCost(0))
	require.Equal(t, "$0.04", formatCost(0.0425))
	require.Equal(t, "$1.23", formatCost(1.234))
	require.Equal(t, "$123.46", formatCost(123.456))
}

// A resting session must show no rate at all: a "~0" would be noise on top of
// the activity word, which already says the session is idle.
func TestFormatRateQuietIsEmpty(t *testing.T) {
	require.Equal(t, "", formatRate(0))
	require.Equal(t, "", formatRate(0.4))
}

// The cell carries no unit — that lives in the column header, so the cells stay
// as narrow as their numbers.
func TestFormatRateHasNoUnit(t *testing.T) {
	require.Equal(t, "~1", formatRate(1))
	require.Equal(t, "~12.3k", formatRate(12_345))
	require.NotContains(t, formatRate(12_345), rateUnit)
}

// A card has no header, so it must spell the unit itself — otherwise its total
// and its rate are two token counts with nothing to tell them apart.
func TestCardLabelSpellsRateUnit(t *testing.T) {
	got := telemetryLabel(&daemon.Telemetry{CostUSD: 1.23, Tokens: 45_600, TokensPerMin: 12_345})
	require.Contains(t, got, "~12.3k"+rateUnit)
	require.Equal(t, "$1.23 · 45.6k · ~12.3k/m", got)
}

// A container that never reported must render blank cells, not zeros: a script
// reading the plain listing has to be able to tell "not measured" from a
// measured zero.
func TestTelemetryCellsUnreportedIsBlank(t *testing.T) {
	cost, tokens, rate := telemetryCells(nil)
	require.Equal(t, "", cost)
	require.Equal(t, "", tokens)
	require.Equal(t, "", rate)
	require.Equal(t, "", telemetryLabel(nil))
}

// A measured zero DOES render, so an instrumented container that has not spent
// anything is visibly distinct from one that never reported.
func TestTelemetryCellsMeasuredZeroRenders(t *testing.T) {
	cost, tokens, rate := telemetryCells(&daemon.Telemetry{})
	require.Equal(t, "$0.00", cost)
	require.Equal(t, "0", tokens)
	require.Equal(t, "", rate, "a zero rate is still dropped")
}

// The plain listing's columns must stay aligned with lsHeaders, since scripts
// parse it positionally.
func TestLsPlainColumnsMatchHeaders(t *testing.T) {
	var b strings.Builder
	err := renderLsPlain(&b, [][]string{
		{"web", "w", "aaaaaa", "ready", "2.1.191", "~/src/web", "working", "$1.23", "45.6k", "~12.3k", "a title"},
	})
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	require.Len(t, lines, 2)
	require.Len(t, strings.Fields(lines[0]), len(lsHeaders)+1, "LOCAL FOLDER is two words")
	require.Contains(t, lines[1], "$1.23")
	require.Contains(t, lines[1], "45.6k")
	require.Contains(t, lines[1], "~12.3k")
}

// On a card the numbers sit between the activity and the title, so a long title
// can never push them off the right edge of the terminal.
func TestCardTelemetryPrecedesTitle(t *testing.T) {
	line := cardState(daemon.Item{
		Status:   daemon.StatusReady,
		Activity: daemon.ActivityWorking,
		Title:    "some long conversation title",
		Telemetry: &daemon.Telemetry{
			CostUSD: 1.23, Tokens: 45_600, TokensPerMin: 12_345,
		},
	})

	require.Contains(t, line, "$1.23 · 45.6k · ~12.3k/m")
	require.Less(t, strings.Index(line, "$1.23"), strings.Index(line, "some long"),
		"consumption must precede the free-form title")
	require.Less(t, strings.Index(line, "working"), strings.Index(line, "$1.23"))
}

// A card for a container that never reported must look exactly as it did before
// telemetry existed — no stray separator, no zeros.
func TestCardWithoutTelemetryUnchanged(t *testing.T) {
	it := daemon.Item{Status: daemon.StatusReady, Activity: daemon.ActivityWaiting, Title: "hi"}
	require.Equal(t, "◦ waiting  hi", cardState(it))
}
