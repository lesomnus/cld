// Package tmuxx manages sessions on a dedicated host tmux server,
// isolated from the user's default server by its own socket.
package tmuxx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type Server struct {
	// Socket is the path of the dedicated tmux server socket.
	Socket string
}

func (s *Server) run(ctx context.Context, args ...string) (string, error) {
	argv := append([]string{"-S", s.Socket}, args...)
	cmd := exec.CommandContext(ctx, "tmux", argv...)
	// The server inherits the environment of whichever tmux invocation starts
	// it; force a UTF-8 locale so it never comes up in "C" (where non-ASCII
	// renders as "_"). A pre-existing LC_ALL must be dropped first — glibc
	// getenv returns the first match, so a plain append would be shadowed.
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LC_ALL=") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "LC_ALL=C.UTF-8")
	out, err := cmd.CombinedOutput()
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
// The command is passed as a single shell command string. remain-on-exit is
// enabled so a session whose command exits (the user ends claude) stays
// attachable, showing its final screen, instead of vanishing.
func (s *Server) NewSession(ctx context.Context, name string, command string) error {
	out, err := s.run(ctx, "new-session", "-d", "-s", name, command)
	if err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	// remain-on-exit is a window option, so it needs a window target (a plain
	// session name resolves to its active window; "=name" is a session target
	// and is rejected here).
	if out, err := s.run(ctx, "set-window-option", "-t", name, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("tmux set remain-on-exit: %w: %s", err, out)
	}
	return s.tune(ctx)
}

// tune sets server/global options that make the claude TUI render well:
// synchronized output so redraws reach the outer terminal as atomic frames
// (no tearing over SSH), and mouse reporting for fullscreen-mode scrolling.
// This server is dedicated to claude panes, so setting them globally is safe.
func (s *Server) tune(ctx context.Context) error {
	if out, err := s.run(ctx, "set-option", "-g", "mouse", "on"); err != nil {
		return fmt.Errorf("tmux set mouse: %w: %s", err, out)
	}

	// Append the "sync" (DECSET 2026) feature once; appending on every session
	// would pile up duplicate list entries.
	features, err := s.run(ctx, "show-options", "-sv", "terminal-features")
	if err == nil && strings.Contains(features, "sync") {
		return nil
	}
	if out, err := s.run(ctx, "set-option", "-sa", "terminal-features", ",xterm*:sync"); err != nil {
		return fmt.Errorf("tmux set terminal-features: %w: %s", err, out)
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
