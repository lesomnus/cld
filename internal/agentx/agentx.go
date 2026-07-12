// Package agentx multiplexes SSH-agent connections over a single duplex byte
// stream (a `docker exec`), so a container can reach an ssh-agent that lives
// on the daemon's side. The container runs a listener on a unix socket; each
// accepted connection becomes a stream, framed over the exec and bridged to a
// freshly-dialed agent socket on the other end.
package agentx

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

type ftype byte

const (
	fOpen  ftype = 1 // a new stream was accepted (originator -> peer)
	fData  ftype = 2 // payload for a stream
	fClose ftype = 3 // a stream ended
)

const (
	chunk = 32 * 1024
	// maxFrame caps a single frame's payload so a desynced/corrupt length can
	// never trigger a huge allocation. Agent messages are far smaller.
	maxFrame = 1 << 20
)

// stream is one multiplexed connection. Its net.Conn may not exist yet (the
// daemon dials asynchronously), so writes buffer until it is attached.
type stream struct {
	mu      sync.Mutex
	conn    net.Conn
	pending [][]byte
	closed  bool
}

func (s *stream) write(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.conn != nil {
		s.conn.Write(p)
		return
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	s.pending = append(s.pending, cp)
}

func (s *stream) attach(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return false
	}
	for _, p := range s.pending {
		c.Write(p)
	}
	s.pending = nil
	s.conn = c
	return true
}

func (s *stream) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.pending = nil
	if s.conn != nil {
		s.conn.Close()
	}
}

// mux frames streams onto w and tracks them by id.
type mux struct {
	wmu sync.Mutex
	w   io.Writer

	mu      sync.Mutex
	streams map[uint32]*stream
}

func newMux(w io.Writer) *mux {
	return &mux{w: w, streams: map[uint32]*stream{}}
}

func (m *mux) send(id uint32, t ftype, p []byte) error {
	var h [9]byte
	binary.BigEndian.PutUint32(h[0:], id)
	h[4] = byte(t)
	binary.BigEndian.PutUint32(h[5:], uint32(len(p)))

	m.wmu.Lock()
	defer m.wmu.Unlock()
	if _, err := m.w.Write(h[:]); err != nil {
		return err
	}
	if len(p) > 0 {
		_, err := m.w.Write(p)
		return err
	}
	return nil
}

func (m *mux) put(id uint32, s *stream) {
	m.mu.Lock()
	m.streams[id] = s
	m.mu.Unlock()
}

func (m *mux) get(id uint32) *stream {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams[id]
}

func (m *mux) drop(id uint32) *stream {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.streams[id]
	delete(m.streams, id)
	return s
}

func (m *mux) closeAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.streams {
		s.close()
		delete(m.streams, id)
	}
}

// pump copies a connection's bytes into Data frames, then a Close frame.
func (m *mux) pump(id uint32, c net.Conn) {
	buf := make([]byte, chunk)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if serr := m.send(id, fData, buf[:n]); serr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	if s := m.drop(id); s != nil {
		s.close()
	}
	m.send(id, fClose, nil)
}

// demux reads frames from r and dispatches them. onOpen, if set, handles a
// peer-originated stream.
func (m *mux) demux(r io.Reader, onOpen func(id uint32)) error {
	hdr := make([]byte, 9)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			return err
		}
		id := binary.BigEndian.Uint32(hdr[0:])
		t := ftype(hdr[4])
		n := binary.BigEndian.Uint32(hdr[5:])
		if n > maxFrame {
			return fmt.Errorf("agentx: frame too large: %d", n)
		}

		var p []byte
		if n > 0 {
			p = make([]byte, n)
			if _, err := io.ReadFull(r, p); err != nil {
				return err
			}
		}

		switch t {
		case fOpen:
			if onOpen != nil {
				onOpen(id)
			}
		case fData:
			if s := m.get(id); s != nil {
				s.write(p)
			}
		case fClose:
			if s := m.drop(id); s != nil {
				s.close()
			}
		}
	}
}

// ListenAndServe runs the container side: it accepts connections on a unix
// socket at path and multiplexes them over out (frames to the daemon) and in
// (frames from the daemon). It returns when in closes or the context is done.
func ListenAndServe(ctx context.Context, path string, in io.Reader, out io.Writer) error {
	// Create the parent dir so callers need not pre-make it (and a transient
	// mkdir failure is retried with the listener rather than being fatal).
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	os.Chmod(path, 0o600)
	return Serve(ctx, ln, in, out)
}

// Serve is ListenAndServe over an already-bound listener, for callers that need
// a non-unix transport — e.g. the API proxy listens on a loopback TCP port so a
// session can point ANTHROPIC_BASE_URL (an http:// URL) at it. It closes ln on
// return.
func Serve(ctx context.Context, ln net.Listener, in io.Reader, out io.Writer) error {
	defer ln.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	m := newMux(out)
	defer m.closeAll()

	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	// Incoming frames carry only Data/Close for our streams; when the daemon
	// side closes (stdin EOF) stop accepting so this process exits instead of
	// orphaning the listener.
	go func() {
		m.demux(in, nil)
		cancel()
	}()

	var next atomic.Uint32
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		id := next.Add(1)
		m.put(id, &stream{conn: c})
		if err := m.send(id, fOpen, nil); err != nil {
			c.Close()
			return err
		}
		go m.pump(id, c)
	}
}

// Bridge runs the daemon side: it reads frames from the container (fromCtr)
// and, for each opened stream, dials a fresh agent connection via dial and
// bridges it, writing return frames to toCtr. The dial runs in its own
// goroutine (early Data is buffered) so a slow dial never stalls other
// streams.
func Bridge(ctx context.Context, toCtr io.Writer, fromCtr io.Reader, dial func(context.Context) (net.Conn, error)) error {
	m := newMux(toCtr)
	defer m.closeAll()

	onOpen := func(id uint32) {
		s := &stream{}
		m.put(id, s)
		go func() {
			c, err := dial(ctx)
			if err != nil {
				if m.drop(id) != nil {
					s.close()
				}
				m.send(id, fClose, nil) // tell the container to close its end
				return
			}
			if !s.attach(c) {
				return // already closed
			}
			m.pump(id, c)
		}()
	}
	return m.demux(fromCtr, onOpen)
}
