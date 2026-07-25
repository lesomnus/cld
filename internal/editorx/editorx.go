// Package editorx launches the user's interactive editor on a file, the way
// `git commit` and `kubectl edit` do: honor $VISUAL then $EDITOR, falling back
// to a common editor found on PATH.
package editorx

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// fallbacks are tried, in order, when neither $VISUAL nor $EDITOR is set; the
// first one on PATH wins, else the last is used and left to fail with a clear
// "executable not found" from exec.
var fallbacks = []string{"nano", "vim", "vi"}

// Resolve returns the editor command and its arguments. It reads $VISUAL then
// $EDITOR (each may carry flags, e.g. "code --wait"), and otherwise picks the
// first fallback editor present on PATH. The returned slice always has at least
// one element (the program).
func Resolve() []string {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			if fields := strings.Fields(v); len(fields) > 0 {
				return fields
			}
		}
	}
	for _, cand := range fallbacks {
		if _, err := exec.LookPath(cand); err == nil {
			return []string{cand}
		}
	}
	return []string{fallbacks[len(fallbacks)-1]}
}

// Open launches the resolved editor on path, wired to the current terminal, and
// blocks until it exits. A non-nil error is either a start failure (editor not
// found) or the editor's own non-zero exit — callers that treat a non-zero exit
// as "user cancelled" (like vim's :cq) can check for *exec.ExitError.
func Open(ctx context.Context, path string) error {
	ed := Resolve()
	argv := append(append([]string{}, ed[1:]...), path)
	cmd := exec.CommandContext(ctx, ed[0], argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
