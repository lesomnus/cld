package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/agentx"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
)

func NewCmdAgent() *xli.Command {
	return &xli.Command{
		Name:     "agent",
		Brief:    "expose the host ssh-agent to cld sessions",
		Commands: []*xli.Command{new_cmd_agent_export()},
		Handler:  xli.RequireSubcommand(),
	}
}

func new_cmd_agent_export() *xli.Command {
	return &xli.Command{
		Name:  "export",
		Brief: "serve the host ssh-agent on the cld socket for containers to reach",
		Flags: flg.Flags{
			&flg.String{Name: "socket", Brief: "socket to serve on (default: cache/agent.sock)"},
			&flg.String{Name: "source", Brief: "file holding the host agent socket path (default: cache/agent.source)"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			listen := c.AgentSocketPath()
			flg.VisitP(cmd, "socket", &listen)
			srcfile := c.AgentSourcePath()
			flg.VisitP(cmd, "source", &srcfile)

			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()

			source := func() (string, error) {
				b, err := os.ReadFile(srcfile)
				return strings.TrimSpace(string(b)), err
			}
			err := agentx.ExportServe(ctx, listen, source)
			if errors.Is(err, agentx.ErrAlreadyServing) || err == context.Canceled {
				return nil
			}
			return err
		}),
	}
}

// prepareHostShare stages host-side git integration into the cache dir before
// an attach: the current login session's ssh-agent (for forwarding) and a copy
// of ~/.gitconfig (so container git matches the host, like VS Code). Best
// effort: any failure just means that piece is unavailable.
func prepareHostShare(c *config.Config) {
	if err := os.MkdirAll(c.CacheDir, 0o755); err != nil {
		return
	}
	stageGitConfig(c)
	stageClaudeConfig(c)
	startAgentExport(c)
}

func stageGitConfig(c *config.Config) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(home, ".gitconfig"))
	if err != nil {
		return
	}
	os.WriteFile(c.GitConfigPath(), data, 0o644)
}

// claudeShareFiles and claudeShareDirs are the host ~/.claude items propagated
// into sessions: user settings and memory, and the customization directories.
// Credentials, project history, and runtime state are deliberately excluded.
var (
	claudeShareFiles = []string{"settings.json", "CLAUDE.md"}
	claudeShareDirs  = []string{"commands", "agents", "output-styles"}
)

// stageClaudeConfig mirrors the host's shared Claude Code config into the cache
// dir for the daemon to install into each session. The stage is rebuilt each
// time so a file removed on the host stops propagating. Best effort.
func stageClaudeConfig(c *config.Config) {
	if !c.Auth.ShareConfigEnabled() {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	src := filepath.Join(home, ".claude")
	dst := c.ClaudeShareDir()

	// Rebuild from scratch so stale (host-deleted) entries do not linger.
	os.RemoveAll(dst)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return
	}
	for _, name := range claudeShareFiles {
		if data, err := os.ReadFile(filepath.Join(src, name)); err == nil {
			os.WriteFile(filepath.Join(dst, name), data, 0o644)
		}
	}
	for _, name := range claudeShareDirs {
		copyTree(filepath.Join(src, name), filepath.Join(dst, name))
	}
}

// copyTree copies a directory tree (regular files only), best effort. A missing
// source is a no-op. Symlinks are skipped, not followed, so a link planted under
// a shared dir cannot pull an arbitrary host file into the stage.
func copyTree(src, dst string) {
	filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		out := filepath.Join(dst, rel)
		if os.MkdirAll(filepath.Dir(out), 0o755) == nil {
			os.WriteFile(out, data, 0o644)
		}
		return nil
	})
}

func startAgentExport(c *config.Config) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" || !c.Auth.ForwardAgentEnabled() {
		return
	}
	os.WriteFile(c.AgentSourcePath(), []byte(sock), 0o600)

	exe, err := os.Executable()
	if err != nil {
		return
	}
	args := []string{"agent", "export"}
	if p := c.Path(); p != "" {
		args = append([]string{"--config", p}, args...)
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		// Reap it: a duplicate spawn exits immediately (ErrAlreadyServing),
		// and without a Wait it would linger as a zombie for our lifetime.
		go cmd.Wait()
	}
	// Give the listener a moment to bind before the caller dials it.
	time.Sleep(150 * time.Millisecond)
}
