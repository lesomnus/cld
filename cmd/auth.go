package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
)

// maxTokenInput bounds how much we read from stdin for a token; the daemon
// enforces its own limit too. A token is a few hundred bytes.
const maxTokenInput = 8192

// maxCredentialsInput bounds how much we read from stdin for a credentials file.
const maxCredentialsInput = 16384

func NewCmdAuth() *xli.Command {
	return &xli.Command{
		Name:  "auth",
		Brief: "manage the Claude Code credentials cld injects into sessions",
		Commands: []*xli.Command{
			newCmdAuthSetToken(),
			newCmdAuthLogin(),
		},
	}
}

// newCmdAuthLogin mints a Claude Code login that the daemon owns from birth, so
// the broker can share it across sessions with no risk to the user's own login.
// It runs `claude auth login` against a throwaway config dir and, when the login
// writes its credentials there, hands them to the daemon and wipes the dir. The
// host's ~/.claude is never read or touched, and the token is a SEPARATE lineage
// from any the user holds — so unlike importing an existing credentials file,
// there is nothing to move, delete, or collide with. Backs the common case:
//
//	cld auth login             # interactive Claude subscription login
//	cld auth login --from PATH # skip the login; import an existing credentials
//	                           #   file, MOVING it (deletes the source) so the
//	                           #   host and daemon never share one refresh token
//
// It is Claude-subscription only: the broker refreshes at the subscription token
// endpoint, and API-billing (Console) users authenticate with an ANTHROPIC_API_KEY
// that needs no broker.
func newCmdAuthLogin() *xli.Command {
	return &xli.Command{
		Name:  "login",
		Brief: "log in and hand the credentials to cld's broker (host ~/.claude untouched)",
		Flags: flg.Flags{
			&flg.String{Name: "from", Brief: "import an existing credentials file instead of logging in (moves it)"},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			if from, ok := flg.Get[string](cmd, "from"); ok && from != "" {
				return importCredentialsFile(ctx, cmd, c.SocketPath(), from)
			}

			creds, err := loginToTempConfig(ctx, cmd)
			if err != nil {
				return err
			}
			if err := daemon.SetCredentials(ctx, c.SocketPath(), creds); err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrWriter, "cld: broker login stored (host ~/.claude untouched). "+
				"Apply to a running session now with `cld it --new`.")
			return nil
		}),
	}
}

// loginToTempConfig runs `claude auth login` with a throwaway CLAUDE_CONFIG_DIR,
// wired to the user's terminal so they can complete the browser flow, and
// returns the credentials it writes there. The dir (and the tokens in it) is
// always wiped before returning. A watchdog stops the process once the
// credentials land, in case the login lingers instead of exiting on its own.
func loginToTempConfig(ctx context.Context, cmd *xli.Command) (string, error) {
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		return "", fmt.Errorf("`claude` not found on PATH; install Claude Code, or import an existing " +
			"credentials file with `cld auth login --from <path>`")
	}

	dir, err := os.MkdirTemp("", "cld-login-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir) // best-effort wipe of the throwaway tokens
	credPath := filepath.Join(dir, ".credentials.json")

	lc := exec.CommandContext(ctx, claudeBin, "auth", "login", "--claudeai")
	lc.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+dir)
	lc.Stdin, lc.Stdout, lc.Stderr = os.Stdin, os.Stdout, os.Stderr

	// Once the credentials file appears, give the login a moment to finish and
	// then stop it if it is still running — some flows drop into a session rather
	// than exiting. The happy path is `claude auth login` exiting on its own,
	// which makes this a no-op.
	watch, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	go func() {
		for {
			select {
			case <-watch.Done():
				return
			case <-time.After(300 * time.Millisecond):
				if fi, err := os.Stat(credPath); err == nil && fi.Size() > 0 {
					time.Sleep(time.Second)
					if lc.Process != nil {
						lc.Process.Signal(os.Interrupt)
					}
					return
				}
			}
		}
	}()

	// Ignore the run error when the credentials were written: a watchdog signal
	// (or the user quitting after login) surfaces as a non-zero exit even though
	// the login succeeded.
	runErr := lc.Run()

	raw, err := os.ReadFile(credPath)
	if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
		if runErr != nil {
			return "", fmt.Errorf("claude auth login did not complete: %w", runErr)
		}
		return "", fmt.Errorf("login finished but wrote no credentials")
	}
	if len(raw) > maxCredentialsInput {
		return "", fmt.Errorf("credentials are too large")
	}
	return strings.TrimSpace(string(raw)), nil
}

// importCredentialsFile MOVES an existing credentials file into the daemon: it
// stores the login and then deletes the source, because the host and the daemon
// must not share one refresh token — whichever refreshes first would lock the
// other out. Deleting the source leaves the host to `/login` again for its own.
func importCredentialsFile(ctx context.Context, cmd *xli.Command, socket string, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read credentials file %q: %w", path, err)
	}
	if len(raw) > maxCredentialsInput {
		return fmt.Errorf("credentials file %q is too large", path)
	}
	creds := strings.TrimSpace(string(raw))
	if creds == "" {
		return fmt.Errorf("credentials file %q is empty", path)
	}
	if err := daemon.SetCredentials(ctx, socket, creds); err != nil {
		return err
	}
	// Only delete after a successful store, so a rejected import never leaves the
	// host with no login at all.
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("stored the login but could not delete %q — remove it and `/login` again "+
			"on the host to avoid a refresh-token clash: %w", path, err)
	}
	fmt.Fprintf(cmd.ErrWriter, "cld: login moved into cld; %s deleted. "+
		"`/login` on the host again for a separate token. Apply now with `cld it --new`.\n", path)
	return nil
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
