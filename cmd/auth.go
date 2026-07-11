package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
)

// maxTokenInput bounds how much we read from stdin for a token; the daemon
// enforces its own limit too. A token is a few hundred bytes.
const maxTokenInput = 8192

func NewCmdAuth() *xli.Command {
	return &xli.Command{
		Name:  "auth",
		Brief: "manage the Claude Code credentials cld injects into sessions",
		Commands: []*xli.Command{
			newCmdAuthSetToken(),
		},
	}
}

// newCmdAuthSetToken hands the daemon a long-lived OAuth token (from
// `claude setup-token`) to inject into every session as CLAUDE_CODE_OAUTH_TOKEN.
// The token is read from stdin so it never appears in argv, the shell history,
// or the process table. Works from inside a devcontainer via the control-API
// relay, so there is no host file to place.
func newCmdAuthSetToken() *xli.Command {
	return &xli.Command{
		Name:  "set-token",
		Brief: "store a Claude Code OAuth token (read from stdin) for cld to inject",
		Flags: flg.Flags{},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			raw, err := io.ReadAll(io.LimitReader(cmd.ReadCloser, maxTokenInput+1))
			if err != nil {
				return fmt.Errorf("read token from stdin: %w", err)
			}
			if len(raw) > maxTokenInput {
				return fmt.Errorf("token is too long")
			}
			token := strings.TrimSpace(string(raw))
			if token == "" {
				return fmt.Errorf("no token on stdin (pipe it: `cld auth set-token < token.txt`)")
			}

			if err := daemon.SetOAuthToken(ctx, c.SocketPath(), token); err != nil {
				return err
			}
			// Never echo the token. Running sessions keep their injected token;
			// a new one is picked up on the next session start.
			fmt.Fprintln(cmd.ErrWriter, "cld: OAuth token stored; new sessions will use it "+
				"(recreate a running session with `cld it --new` to apply it now)")
			return nil
		}),
	}
}
