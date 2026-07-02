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

// Run watches root until the context is done. If root does not exist yet
// it waits for it to appear.
func Run(ctx context.Context, root string, out io.Writer) error {
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

	if err := add_tree(w, root); err != nil {
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
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					add_tree(w, ev.Name)
				}
			}
			rel, err := filepath.Rel(root, ev.Name)
			if err != nil {
				continue
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

func add_tree(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			w.Add(p)
		}
		return nil
	})
}
