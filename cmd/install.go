package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/cld/internal/installer"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
	"github.com/moby/moby/client"
	"golang.org/x/term"
)

func NewCmdInstall() *xli.Command {
	return &xli.Command{
		Name:  "install",
		Brief: "run the cld daemon as a container on this host's Docker",
		Flags: flg.Flags{
			&flg.Switch{Name: "recreate", Brief: "replace an existing daemon container"},
			&flg.String{Name: "image", Brief: "daemon image (default: config's install.image)"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			image := c.Install.Image
			flg.VisitP(cmd, "image", &image)
			recreate, _ := flg.Get[bool](cmd, "recreate")

			// Pre-create the shared dirs as this user so the daemon's sockets
			// and backups land under paths the host `cld` owns and reaches.
			os.MkdirAll(c.CacheDir, 0o755)
			os.MkdirAll(c.DataDir, 0o755)

			// Offer to set the OAuth token before the daemon starts, so a fresh
			// install is authenticated with no extra step. Only when none is
			// configured and stdin is interactive; a scripted install proceeds
			// and can set one later with `cld auth set-token`.
			if err := prompt_oauth_token(cmd, c); err != nil {
				return err
			}

			spec, err := installer.SpecFor(image, os.Getenv("DOCKER_HOST"), c.CacheDir, c.DataDir, os.Getuid(), os.Getgid())
			if err != nil {
				return err
			}

			cli, err := client.New(client.FromEnv)
			if err != nil {
				return z.Err(err, "docker client")
			}
			defer cli.Close()

			id, err := installer.Install(ctx, cli, spec, recreate, cmd.ErrWriter)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrWriter, "cld: daemon started (%s)\n", short_id(id))

			// The daemon writes its socket into the shared cache dir; wait for it
			// so `cld it`/`cld up` work right after install.
			if wait_daemon(ctx, c.SocketPath(), 30*time.Second) {
				fmt.Fprintln(cmd.ErrWriter, "cld: daemon is ready")
			} else {
				fmt.Fprintln(cmd.ErrWriter, "cld: daemon started but not answering yet; check `cld ls`")
			}
			return nil
		}),
	}
}

// need_oauth_token reports whether install should offer to set a token: only
// when neither a stored token (from a prior `cld auth set-token`) nor a
// configured auth.oauth_token_file is present.
func need_oauth_token(c *config.Config) bool {
	if c.Auth.OAuthTokenFile != "" {
		return false
	}
	_, err := os.Stat(c.OAuthTokenStorePath())
	return os.IsNotExist(err)
}

// prompt_oauth_token asks for a Claude Code OAuth token (from `claude
// setup-token`) and stores it under DataDir so the daemon injects it into every
// session. It is skipped when a token is already configured or when stdin is not
// a terminal (a scripted install must not block). Input is read without echo so
// the secret never appears on screen; an empty entry skips.
func prompt_oauth_token(cmd *xli.Command, c *config.Config) error {
	if !need_oauth_token(c) {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}

	fmt.Fprintln(cmd.ErrWriter, "No Claude Code OAuth token is configured.")
	fmt.Fprintln(cmd.ErrWriter, "Paste one from `claude setup-token` to authenticate every session,")
	fmt.Fprint(cmd.ErrWriter, "or press Enter to skip (set it later with `cld auth set-token`): ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(cmd.ErrWriter) // ReadPassword swallows the newline
	if err != nil {
		// Don't fail the install over a prompt read error; the token is optional.
		fmt.Fprintln(cmd.ErrWriter, "cld: could not read token; continuing without one")
		return nil
	}

	token := strings.TrimSpace(string(raw))
	if token == "" {
		fmt.Fprintln(cmd.ErrWriter, "cld: no token entered; continuing without one")
		return nil
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return fmt.Errorf("token contains whitespace")
	}

	p := c.OAuthTokenStorePath()
	if err := os.WriteFile(p, []byte(token), 0o600); err != nil {
		return fmt.Errorf("store token: %w", err)
	}
	fmt.Fprintln(cmd.ErrWriter, "cld: OAuth token stored")
	return nil
}

func short_id(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// wait_daemon polls the daemon's control socket until it answers or the timeout
// elapses.
func wait_daemon(ctx context.Context, socket string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ictx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := daemon.FetchInfo(ictx, socket)
		cancel()
		if err == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}
