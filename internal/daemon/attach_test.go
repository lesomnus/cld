package daemon

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/lesomnus/cld/internal/termx"
	"github.com/stretchr/testify/require"
)

// frame the client->daemon direction exactly as AttachSession does, so the
// test pins the wire format both ends must agree on.
func attachData_frame(b *bytes.Buffer, s string) {
	b.WriteByte(attachData)
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	b.Write(n[:])
	b.WriteString(s)
}

func attachResize_frame(b *bytes.Buffer, cols, rows uint16) {
	b.WriteByte(attachResize)
	var p [4]byte
	binary.BigEndian.PutUint16(p[:2], cols)
	binary.BigEndian.PutUint16(p[2:], rows)
	b.Write(p[:])
}

func TestReadAttachFrames(t *testing.T) {
	t.Run("interleaves data and resize", func(t *testing.T) {
		var in bytes.Buffer
		attachData_frame(&in, "hello ")
		attachResize_frame(&in, 100, 40)
		attachData_frame(&in, "world")

		var data bytes.Buffer
		resize := make(chan termx.Size, 8)
		err := read_attach_frames(bytes.NewReader(in.Bytes()), &data, resize)

		require.ErrorIs(t, err, io.EOF)
		require.Equal(t, "hello world", data.String())
		require.Len(t, resize, 1)
		require.Equal(t, termx.Size{Cols: 100, Rows: 40}, <-resize)
	})

	t.Run("rejects an oversized data frame", func(t *testing.T) {
		var in bytes.Buffer
		in.WriteByte(attachData)
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], maxAttachData+1)
		in.Write(n[:])

		err := read_attach_frames(bytes.NewReader(in.Bytes()), io.Discard, make(chan termx.Size, 1))
		require.Error(t, err)
		require.NotErrorIs(t, err, io.EOF)
	})

	t.Run("rejects an unknown frame type", func(t *testing.T) {
		err := read_attach_frames(bytes.NewReader([]byte{'x'}), io.Discard, make(chan termx.Size, 1))
		require.Error(t, err)
	})

	t.Run("never blocks when the resize consumer is busy", func(t *testing.T) {
		var in bytes.Buffer
		for i := 0; i < 5; i++ {
			attachResize_frame(&in, uint16(80+i), 24)
		}
		// Capacity 1: extra resizes must be dropped, not deadlock.
		err := read_attach_frames(bytes.NewReader(in.Bytes()), io.Discard, make(chan termx.Size, 1))
		require.ErrorIs(t, err, io.EOF)
	})
}
