// Package watchx implements the in-container file watcher: it watches the
// Claude Code config dir recursively with inotify and prints one changed
// path per line to stdout, which the daemon reads over the exec stream.
package watchx

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Run watches root until the context is done, emitting each changed path
// (relative to root) on its own line. If root does not exist yet it waits for
// it to appear. skip, if non-nil, is given paths relative to root; a directory
// it returns true for is not watched and its changes are not emitted, keeping
// high-churn or irrelevant subtrees off inotify and off the stream.
func Run(ctx context.Context, root string, out io.Writer, skip func(rel string) bool) error {
	for {
		if _, err := os.Stat(root); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	if err := add_tree(w, root, root, skip); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			rel, err := filepath.Rel(root, ev.Name)
			if err != nil {
				continue
			}
			if skip != nil && skip(rel) {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					add_tree(w, root, ev.Name, skip)
				}
			}
			fmt.Fprintln(out, rel)

		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintln(os.Stderr, "watch error:", err)
		}
	}
}

func add_tree(w *fsnotify.Watcher, root string, dir string, skip func(rel string) bool) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if skip != nil {
			if rel, err := filepath.Rel(root, p); err == nil && rel != "." && skip(rel) {
				return filepath.SkipDir
			}
		}
		w.Add(p)
		return nil
	})
}
