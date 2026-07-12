package cmd

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// bufWC is a bytes.Buffer that satisfies io.WriteCloser.
type bufWC struct{ bytes.Buffer }

func (*bufWC) Close() error { return nil }

func TestPumpMasked(t *testing.T) {
	run := func(in string) (echo string, forwarded string, interrupted bool) {
		var e bytes.Buffer
		var out bufWC
		pumpMasked(bufio.NewReader(strings.NewReader(in)), &e, &out, func() { interrupted = true })
		return e.String(), out.String(), interrupted
	}

	t.Run("echoes one asterisk per character and forwards the real bytes on enter", func(t *testing.T) {
		echo, fwd, _ := run("ab\r")
		require.Equal(t, "**\r\n", echo)
		require.Equal(t, "ab\n", fwd)
	})

	t.Run("backspace erases a character from both the echo and what is forwarded", func(t *testing.T) {
		echo, fwd, _ := run("abx\x7fc\r")
		require.Equal(t, "***\b \b*\r\n", echo)
		require.Equal(t, "abc\n", fwd)
	})

	t.Run("each newline forwards its own line", func(t *testing.T) {
		_, fwd, _ := run("a\rb\n")
		require.Equal(t, "a\nb\n", fwd)
	})

	t.Run("ctrl-c interrupts without forwarding the partial line", func(t *testing.T) {
		echo, fwd, interrupted := run("ab\x03")
		require.Equal(t, "**\r\n", echo)
		require.Empty(t, fwd)
		require.True(t, interrupted, "ctrl-c must trigger onInterrupt")
	})

	t.Run("ctrl-d ends input as EOF, not an interrupt", func(t *testing.T) {
		echo, fwd, interrupted := run("ab\x04")
		require.Equal(t, "**\r\n", echo)
		require.Empty(t, fwd)
		require.False(t, interrupted, "ctrl-d is EOF, not a cancel")
	})
}

func TestCRLFWriter(t *testing.T) {
	var buf bytes.Buffer
	w := &crlfWriter{w: &buf}

	n, err := w.Write([]byte("x\ny\r\nz\n"))
	require.NoError(t, err)
	require.Equal(t, len("x\ny\r\nz\n"), n) // reports the logical length
	require.Equal(t, "x\r\ny\r\nz\r\n", buf.String())
}
