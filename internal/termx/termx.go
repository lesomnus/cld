// Package termx implements the interactive exec-attach client used as the
// tmux pane command: cld's own "docker exec -it", built on the SDK so the
// host needs no docker CLI.
package termx

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/moby/moby/client"
	"golang.org/x/term"
)

type ExecOptions struct {
	Container  string
	User       string
	WorkingDir string
	Env        []string
	Cmd        []string
}

// Run execs the command in the container with this process's terminal
// attached, handling raw mode and window resizes. It returns the remote
// process's exit code.
func Run(ctx context.Context, cli *client.Client, o ExecOptions) (int, error) {
	fd := int(os.Stdin.Fd())
	is_tty := term.IsTerminal(fd)

	size := client.ConsoleSize{}
	if is_tty {
		if w, h, err := term.GetSize(fd); err == nil {
			size = client.ConsoleSize{Width: uint(w), Height: uint(h)}
		}
	}

	created, err := cli.ExecCreate(ctx, o.Container, client.ExecCreateOptions{
		User:         o.User,
		TTY:          is_tty,
		ConsoleSize:  size,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env:          o.Env,
		WorkingDir:   o.WorkingDir,
		Cmd:          o.Cmd,
	})
	if err != nil {
		return 0, err
	}

	att, err := cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{
		TTY:         is_tty,
		ConsoleSize: size,
	})
	if err != nil {
		return 0, err
	}
	defer att.Close()

	if is_tty {
		old, err := term.MakeRaw(fd)
		if err == nil {
			defer term.Restore(fd, old)
		}

		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				w, h, err := term.GetSize(fd)
				if err != nil {
					continue
				}
				cli.ExecResize(ctx, created.ID, client.ExecResizeOptions{
					Width:  uint(w),
					Height: uint(h),
				})
			}
		}()
		winch <- syscall.SIGWINCH
	}

	go func() {
		io.Copy(att.Conn, os.Stdin)
		if cw, ok := att.Conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()

	// With a TTY the stream is raw; without one it would be multiplexed,
	// but this command only ever runs inside a tmux pane (always a TTY).
	io.Copy(os.Stdout, att.Reader)

	insp, err := cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return 0, err
	}
	return insp.ExitCode, nil
}

// Size is a terminal window size in character cells.
type Size struct {
	Cols uint16
	Rows uint16
}

// Stream execs the command in the container with a TTY and bridges it to in
// and out instead of this process's stdio (as Run does), applying window sizes
// received on resize. It serves an attach whose terminal lives on the far end
// of a socket. It returns the remote process's exit code.
func Stream(ctx context.Context, cli *client.Client, o ExecOptions, in io.Reader, out io.Writer, size Size, resize <-chan Size) (int, error) {
	cs := client.ConsoleSize{Width: uint(size.Cols), Height: uint(size.Rows)}
	created, err := cli.ExecCreate(ctx, o.Container, client.ExecCreateOptions{
		User:         o.User,
		TTY:          true,
		ConsoleSize:  cs,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Env:          o.Env,
		WorkingDir:   o.WorkingDir,
		Cmd:          o.Cmd,
	})
	if err != nil {
		return 0, err
	}

	att, err := cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{TTY: true, ConsoleSize: cs})
	if err != nil {
		return 0, err
	}
	defer att.Close()

	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// ctx cancellation (client disconnect / daemon shutdown) must close att so
	// the output copy below unblocks: a TTY exec does not end on stdin EOF, so
	// nothing else would reap the exec until it happens to emit output.
	go func() {
		<-rctx.Done()
		att.Close()
	}()
	go func() {
		for {
			select {
			case <-rctx.Done():
				return
			case s := <-resize:
				cli.ExecResize(rctx, created.ID, client.ExecResizeOptions{Width: uint(s.Cols), Height: uint(s.Rows)})
			}
		}
	}()

	go func() {
		io.Copy(att.Conn, in)
		if cw, ok := att.Conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()

	// TTY stream is raw (not stdcopy-multiplexed).
	io.Copy(out, att.Reader)

	insp, err := cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return 0, err
	}
	return insp.ExitCode, nil
}
