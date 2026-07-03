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
