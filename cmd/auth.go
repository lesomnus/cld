package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"golang.org/x/term"
)

// maxCredentialsInput bounds how much we read from stdin for a credentials file.
const maxCredentialsInput = 16384

func NewCmdAuth() *xli.Command {
	return &xli.Command{
		Name:  "auth",
		Brief: "manage the broker login cld shares across --proxy sessions",
		Commands: []*xli.Command{
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

			creds, err := loginToTempConfig(ctx)
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
func loginToTempConfig(ctx context.Context) (string, error) {
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

	// Own the process lifetime so an interrupt can force claude down: raw mode
	// (below) swallows Ctrl-C, so cancelling this context is how we stop it.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	lc := exec.CommandContext(runCtx, claudeBin, "auth", "login", "--claudeai")
	lc.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+dir)
	lc.Stdout, lc.Stderr = os.Stdout, os.Stderr
	// claude reads the pasted code with echo off, so a plain passthrough gives the
	// user no feedback at all. When interactive, feed claude's stdin through a pipe
	// and echo one '*' per character ourselves so the entry is visible. In raw mode
	// Ctrl-C is a byte, not a signal, so the pump restores the terminal and cancels
	// the run itself — otherwise a Ctrl-C at the prompt would wedge the terminal.
	var canceled atomic.Bool
	if restore, ok := startMaskedStdin(lc, func() { canceled.Store(true); cancelRun() }); ok {
		defer restore()
	} else {
		lc.Stdin = os.Stdin
	}

	// Once the credentials file appears, give the login a moment to finish and
	// then stop it if it is still running — some flows drop into a session rather
	// than exiting. The happy path is `claude auth login` exiting on its own,
	// which makes this a no-op.
	watch, cancelWatch := context.WithCancel(runCtx)
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

	if canceled.Load() {
		return "", fmt.Errorf("login canceled")
	}
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

// startMaskedStdin makes the pasted login code visible as it is typed. claude
// reads it with echo off, so it hands claude a pipe for stdin and echoes '*' per
// character itself, forwarding the real bytes to claude on Enter. Returns
// ok=false (leaving stdin to the caller) when stdin/stdout is not a terminal.
// Raw mode drops output post-processing, so claude's own prompts are routed
// through a writer that re-adds carriage returns. onInterrupt is called when the
// user hits Ctrl-C — raw mode delivers it as a byte, not a signal, so the caller
// must tear claude down itself. The returned restore is idempotent: the pump
// runs it on interrupt to un-raw the terminal immediately, and the deferred call
// is then a no-op.
func startMaskedStdin(lc *exec.Cmd, onInterrupt func()) (func(), bool) {
	in := int(os.Stdin.Fd())
	if !term.IsTerminal(in) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return nil, false
	}
	old, err := term.MakeRaw(in)
	if err != nil {
		return nil, false
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		term.Restore(in, old)
		return nil, false
	}
	lc.Stdin = pr
	lc.Stdout = &crlfWriter{w: os.Stdout}
	lc.Stderr = &crlfWriter{w: os.Stderr}

	var restoreOnce sync.Once
	restore := func() {
		restoreOnce.Do(func() {
			term.Restore(in, old)
			pw.Close()
			pr.Close()
		})
	}

	go pumpMasked(bufio.NewReader(os.Stdin), os.Stdout, pw, func() {
		restore()     // un-raw the terminal before we kill claude
		onInterrupt() // tear the login down
	})

	return restore, true
}

// pumpMasked reads one line at a time, echoing '*' per character (honoring
// backspace) to echo, then forwards the real bytes to out on Enter — so claude,
// reading a line from its stdin pipe, gets the true code while the screen shows
// only asterisks. On Ctrl-C it calls onInterrupt (the caller tears claude down,
// since raw mode gave us a byte, not a signal) and returns. Ctrl-D and a read
// error both just close out (EOF) so claude's own read ends.
func pumpMasked(r *bufio.Reader, echo io.Writer, out io.WriteCloser, onInterrupt func()) {
	defer out.Close()
	var line []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return
		}
		switch b {
		case '\r', '\n':
			fmt.Fprint(echo, "\r\n")
			if _, err := out.Write(append(line, '\n')); err != nil {
				return
			}
			line = line[:0]
		case 3: // Ctrl-C: raw mode made this a byte, not a signal — cancel the login.
			fmt.Fprint(echo, "\r\n")
			onInterrupt()
			return
		case 4: // Ctrl-D: end claude's read with EOF.
			fmt.Fprint(echo, "\r\n")
			return
		case 127, 8: // Backspace/Delete
			if len(line) > 0 {
				line = line[:len(line)-1]
				fmt.Fprint(echo, "\b \b")
			}
		default:
			line = append(line, b)
			fmt.Fprint(echo, "*")
		}
	}
}

// crlfWriter re-adds a carriage return before a bare newline, so output written
// while the terminal is in raw mode (which drops the ONLCR translation) is not
// stairstepped.
type crlfWriter struct{ w io.Writer }

func (c *crlfWriter) Write(p []byte) (int, error) {
	var b []byte
	for i := range p {
		if p[i] == '\n' && (i == 0 || p[i-1] != '\r') {
			b = append(b, '\r')
		}
		b = append(b, p[i])
	}
	if _, err := c.w.Write(b); err != nil {
		return 0, err
	}
	return len(p), nil
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
