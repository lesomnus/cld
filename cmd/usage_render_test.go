package cmd

import (
	"regexp"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/lesomnus/cld/internal/daemon"
	"github.com/stretchr/testify/require"
)

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func TestUsageGaugeWidth(t *testing.T) {
	// The gauge is always exactly `cells` display columns wide, whatever the
	// percentage, so right-aligned bars stay aligned down the column.
	for _, pct := range []float64{0, 8, 17, 50, 99.9, 100, 150, -5} {
		require.Equal(t, 8, lipgloss.Width(usageGauge(pct, 8)), "pct=%v", pct)
	}
}

func TestUsageGaugeEnds(t *testing.T) {
	// 0% is all baseline track; 100% is all full cells.
	require.Equal(t, "⣀⣀⣀⣀", stripANSI(usageGauge(0, 4)))
	require.Equal(t, "⣿⣿⣿⣿", stripANSI(usageGauge(100, 4)))
}

func TestUsageRemaining(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	require.Equal(t, "", usageRemaining(time.Time{}, now))
	require.Equal(t, "0m", usageRemaining(now.Add(-time.Minute), now))
	require.Equal(t, "45m", usageRemaining(now.Add(45*time.Minute), now))
	require.Equal(t, "2h05m", usageRemaining(now.Add(2*time.Hour+5*time.Minute), now))
	require.Equal(t, "5d23h", usageRemaining(now.Add(5*24*time.Hour+23*time.Hour), now))
}

func TestUsageBarError(t *testing.T) {
	line := usageBar(daemon.UsageSource{Label: "x", Error: "boom"}, time.Now())
	require.Contains(t, stripANSI(line), "⚠")
}
