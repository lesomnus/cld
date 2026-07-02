// Package tmuxx manages sessions on a dedicated host tmux server,
// isolated from the user's default server by its own socket.
package tmuxx

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type Server struct {
	// Socket is the path of the dedicated tmux server socket.
	Socket string
}

func (s *Server) run(ctx context.Context, args ...string) (string, error) {
	argv := append([]string{"-S", s.Socket}, args...)
	out, err := exec.CommandContext(ctx, "tmux", argv...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (s *Server) HasSession(ctx context.Context, name string) (bool, error) {
	_, err := s.run(ctx, "has-session", "-t", "="+name)
	if err == nil {
		return true, nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		// Also the "no server running" case; either way the session is absent.
		return false, nil
	}
	return false, err
}

// NewSession creates a detached session running the given command line.
// The command is passed as a single shell command string.
func (s *Server) NewSession(ctx context.Context, name string, command string) error {
	out, err := s.run(ctx, "new-session", "-d", "-s", name, command)
	if err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	return nil
}

func (s *Server) KillSession(ctx context.Context, name string) error {
	out, err := s.run(ctx, "kill-session", "-t", "="+name)
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return nil
		}
		return fmt.Errorf("tmux kill-session: %w: %s", err, out)
	}
	return nil
}

func (s *Server) ListSessions(ctx context.Context) ([]string, error) {
	out, err := s.run(ctx, "list-sessions", "-F", "#S")
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			// No server running.
			return nil, nil
		}
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// AttachArgv is the argv to attach to a session, suitable for exec(2).
func (s *Server) AttachArgv(name string) []string {
	return []string{"tmux", "-S", s.Socket, "attach-session", "-t", "=" + name}
}

// Quote shell-quotes a single argument with single quotes.
func Quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteAll builds a shell command line from argv.
func QuoteAll(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = Quote(a)
	}
	return strings.Join(parts, " ")
}
