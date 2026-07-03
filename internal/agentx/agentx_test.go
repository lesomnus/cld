package agentx

import (
	"context"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// echoAgent is a stand-in for ssh-agent: it echoes each request back, so the
// test can assert bytes made the full round trip through the mux.
func echoAgent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(c, c)
		}
	}()
	return sock
}

func TestRoundTrip(t *testing.T) {
	agent := echoAgent(t)
	ctrSock := filepath.Join(t.TempDir(), "ctr.sock")

	// Two in-memory pipes model the exec's duplex stream: what the container
	// writes, the daemon reads, and vice versa.
	ctrOutR, ctrOutW := io.Pipe() // container -> daemon
	daeOutR, daeOutW := io.Pipe() // daemon -> container

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ListenAndServe(ctx, ctrSock, daeOutR, ctrOutW)
	go Bridge(ctx, daeOutW, ctrOutR, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", agent)
	})

	// Wait for the container listener to come up.
	var conn net.Conn
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.Dial("unix", ctrSock)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NoError(t, err)

	t.Run("bytes echo through the relay", func(t *testing.T) {
		msg := []byte("SSH-AGENT-REQUEST-0123456789")
		_, err := conn.Write(msg)
		require.NoError(t, err)
		got := make([]byte, len(msg))
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, err = io.ReadFull(conn, got)
		require.NoError(t, err)
		require.Equal(t, msg, got)
	})

	t.Run("concurrent streams stay independent", func(t *testing.T) {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				c, err := net.Dial("unix", ctrSock)
				if err != nil {
					t.Errorf("dial: %v", err)
					return
				}
				defer c.Close()
				msg := []byte(fmt.Sprintf("stream-%d-payload", i))
				c.Write(msg)
				got := make([]byte, len(msg))
				c.SetReadDeadline(time.Now().Add(3 * time.Second))
				if _, err := io.ReadFull(c, got); err != nil {
					t.Errorf("read: %v", err)
					return
				}
				if string(got) != string(msg) {
					t.Errorf("got %q want %q", got, msg)
				}
			}(i)
		}
		wg.Wait()
	})
}

func TestBridgeDialFailureClosesStream(t *testing.T) {
	ctrSock := filepath.Join(t.TempDir(), "ctr.sock")
	ctrOutR, ctrOutW := io.Pipe()
	daeOutR, daeOutW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go ListenAndServe(ctx, ctrSock, daeOutR, ctrOutW)
	go Bridge(ctx, daeOutW, ctrOutR, func(context.Context) (net.Conn, error) {
		return nil, fmt.Errorf("no agent")
	})

	var conn net.Conn
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err = net.Dial("unix", ctrSock)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NoError(t, err)

	// With no agent to bridge to, the daemon closes the stream, so the
	// client's connection reaches EOF instead of hanging.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
}
