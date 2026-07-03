package agentx

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
)

// ErrAlreadyServing is returned by ExportServe when another process already
// serves the listen socket, so callers can treat a duplicate spawn as success.
var ErrAlreadyServing = errors.New("agent export already running")

// ExportServe runs the host-side exporter: it serves an ssh-agent on `listen`
// and forwards each connection to the socket returned by source(), resolved
// fresh per connection so a new login session's agent is picked up without a
// restart. This bridges the host agent to the daemon over a stable path (and,
// for a compose daemon, over the shared cache dir).
func ExportServe(ctx context.Context, listen string, source func() (string, error)) error {
	// An exclusive lock makes the singleton race-free: only one exporter binds
	// the socket, even when several `cld it`/`up` spawns start at once.
	lock, err := os.OpenFile(listen+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return ErrAlreadyServing
	}
	defer lock.Close() // held for the process lifetime; released on exit

	os.Remove(listen)
	ln, err := net.Listen("unix", listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	os.Chmod(listen, 0o600)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go exportForward(c, source)
	}
}

func exportForward(down net.Conn, source func() (string, error)) {
	defer down.Close()

	src, err := source()
	if err != nil || src == "" {
		return
	}
	up, err := net.Dial("unix", src)
	if err != nil {
		return
	}
	defer up.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(up, down); done <- struct{}{} }()
	go func() { io.Copy(down, up); done <- struct{}{} }()
	<-done
}
