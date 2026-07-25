package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/lesomnus/cld/internal/editorx"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/tab"
)

func new_cmd_settings_edit() *xli.Command {
	return &xli.Command{
		Name:  "edit",
		Brief: "edit the Claude Code config cld installs into every devcontainer",
		Args: arg.Args{
			&arg.String{Name: "file", Brief: "which user-default file: `settings` (default) or `claude-md`", Optional: true, Handler: completeEditTargets()},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			which, _ := arg.Get[string](cmd, "file")
			t, err := resolveEditTarget(which)
			if err != nil {
				return err
			}

			// The user-default dir is cld's own (not the host's ~/.claude); create
			// it on first edit so a fresh install can seed a config here.
			dir := c.UserDefaultDir()
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dir, err)
			}

			return editUserDefault(ctx, cmd, filepath.Join(dir, t.name), t)
		}),
	}
}

// editTarget is one editable user-default file. object marks a file that must
// parse as a JSON object (settings.json — cld drops anything else at install),
// so a broken edit is caught before it is saved instead of being silently
// ignored inside every container.
type editTarget struct {
	name     string
	ext      string
	object   bool
	template []byte
}

func resolveEditTarget(which string) (editTarget, error) {
	switch which {
	case "", "settings", "settings.json":
		return editTarget{name: "settings.json", ext: ".json", object: true, template: []byte("{\n}\n")}, nil
	case "claude-md", "claude.md", "CLAUDE.md", "memory":
		return editTarget{name: "CLAUDE.md", ext: ".md", template: []byte{}}, nil
	default:
		return editTarget{}, fmt.Errorf("unknown file %q: use `settings` or `claude-md`", which)
	}
}

func completeEditTargets() arg.Handler[string] {
	return arg.OnTab[string](func(ctx context.Context, t tab.Tab) {
		t.ValueD("settings", "settings.json — user-level Claude Code settings")
		t.ValueD("claude-md", "CLAUDE.md — personal memory added to every session")
	})
}

// editUserDefault runs the kubectl-edit flow against a single host file: copy
// the current content (or a template when it does not exist yet) into a temp
// file, open the user's editor on it, and — only if it changed and, for a JSON
// object file, still parses — write it back atomically. An unchanged buffer or a
// non-zero editor exit (vim's :cq) cancels without touching the file.
func editUserDefault(ctx context.Context, cmd *xli.Command, path string, t editTarget) error {
	orig, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		orig = t.template
	} else if err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "cld-edit-*"+t.ext)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(orig); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// last is the previous attempt's content: when an invalid edit is left
	// unchanged across two rounds, the editor cannot signal a cancel (e.g. nano
	// always exits 0), so give up rather than loop forever — preserving the
	// buffer so no work is lost.
	last := orig
	for {
		if err := editorx.Open(ctx, tmpPath); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				fmt.Fprintln(cmd.ErrWriter, "cld: edit cancelled, no changes made")
				return nil
			}
			return fmt.Errorf("launch editor: %w", err)
		}

		edited, err := os.ReadFile(tmpPath)
		if err != nil {
			return err
		}
		if bytes.Equal(edited, orig) {
			fmt.Fprintln(cmd.ErrWriter, "cld: no changes made")
			return nil
		}

		if t.object {
			if verr := validSettingsObject(edited); verr != nil {
				fmt.Fprintf(cmd.ErrWriter, "cld: %s is not a valid JSON object: %v\n", t.name, verr)
				if bytes.Equal(edited, last) {
					saved := preserveBuffer(edited, t.ext)
					return fmt.Errorf("edit aborted; %s not saved. Your edits are at %s", t.name, saved)
				}
				last = edited
				continue
			}
		}

		if err := writeAtomic(path, edited, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrWriter, "cld: updated %s\n", path)
		fmt.Fprintln(cmd.ErrWriter, "cld: it applies to new or recreated sessions — run `cld it --new <name>` or `cld update` to pick it up now")
		return nil
	}
}

// validSettingsObject reports whether b parses as a JSON object, mirroring what
// claude.SanitizeUserSettings requires: anything else (a list, a scalar, null)
// is dropped at install time, so catching it here turns a silently-ignored file
// into an up-front error.
func validSettingsObject(b []byte) error {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if m == nil {
		return errors.New("expected a JSON object like {\"model\": \"...\"}")
	}
	return nil
}

// writeAtomic writes data to path via a temp file in the same directory and a
// rename, so a crash mid-write never leaves a truncated config that would break
// every session.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".cld-edit-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// preserveBuffer stores an abandoned invalid edit so the user can recover it,
// returning its path. Best-effort: on failure it returns "(discarded)".
func preserveBuffer(data []byte, ext string) string {
	f, err := os.CreateTemp("", "cld-edit-rejected-*"+ext)
	if err != nil {
		return "(discarded)"
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return "(discarded)"
	}
	return f.Name()
}
