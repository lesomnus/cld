package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/cld/internal/claude"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/dockerx"
	"github.com/lesomnus/cld/internal/syncer"
	"github.com/lesomnus/cld/internal/tmuxx"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// lineBuffer is a bounded, concurrency-safe sink for watcher stderr.
type lineBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *lineBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) < 4096 {
		b.buf = append(b.buf, p...)
	}
	return len(p), nil
}

func (b *lineBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func (d *Daemon) layout(e *entry) syncer.Layout {
	return syncer.Layout{
		ProjectDir: d.cfg.ProjectBackupDir(d.backup_key(e)),
	}
}

// backup_key names a project's conversation backup. It is keyed by the
// devcontainer.json "name" (so the history follows the project across path
// moves and machines, and same-named projects intentionally share it),
// namespaced with "cld-". Without a name there is no portable identifier, so
// it falls back to the folder name plus a short path hash to stay unique.
func (d *Daemon) backup_key(e *entry) string {
	if s := devc.Slug(e.dev_name); s != "" {
		return "cld-" + s
	}
	base := devc.Slug(devc.DisplayName(e.item.LocalFolder))
	if base == "" {
		base = "devcontainer"
	}
	return "cld-" + base + "-" + short_hash(e.item.LocalFolder)
}

func short_hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:6]
}

// mark accumulates dirty flags and wakes the sync loop without blocking or
// losing flags when a burst arrives.
func (e *entry) mark(p dirty) {
	e.dirty_mu.Lock()
	e.dirty.settings = e.dirty.settings || p.settings
	e.dirty.transcript = e.dirty.transcript || p.transcript
	e.dirty_mu.Unlock()

	select {
	case e.dirty_sig <- struct{}{}:
	default:
	}
}

// take clears and returns the accumulated dirty flags.
func (e *entry) take() dirty {
	e.dirty_mu.Lock()
	defer e.dirty_mu.Unlock()
	p := e.dirty
	e.dirty = dirty{}
	return p
}

// sync_loop debounces dirty notifications and posts a copy-out to the
// container's worker so it never runs concurrently with a stop/teardown sync.
func (d *Daemon) sync_loop(ctx context.Context, e *entry) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.dirty_sig:
		}

		t := time.NewTimer(d.cfg.Sync.Debounce.Std())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}

		p := e.take()
		if !p.settings && !p.transcript {
			continue
		}
		e.mbox.post(func() { d.copy_out(context.WithoutCancel(ctx), e, p) })
	}
}

// copy_out snapshots container state into the host's per-project backup dir.
// It runs only on the entry's worker (serialized with provisioning and
// teardown), and takes the project's lock so two containers sharing a backup
// key never write it at once. Settings-like state (settings.json, .claude.json,
// skills/, plugins/, ...) is copied out here too, but always into this SAME
// isolated dir — never a bucket shared across projects — so it can only ever
// affect this project's own future restores. cld's own user-default dir,
// mirrored in on every provision via install_claude_config, is still the
// authoritative source for the parts of that state a user sets manually.
func (d *Daemon) copy_out(ctx context.Context, e *entry, p dirty) {
	if !p.settings && !p.transcript {
		return
	}

	// Serialize with any other container writing the same (same-keyed /
	// same-name) project backup dir.
	l := d.proj_locks.get(d.backup_key(e))
	l.Lock()
	defer l.Unlock()

	err := syncer.CopyOut(ctx, d.cli, e.id, e.cfg_dir, d.layout(e), e.item.Workspace, p.settings, p.transcript)
	if err != nil && ctx.Err() == nil {
		d.log.Warn("copy-out failed",
			slog.String("name", e.item.Name), slog.String("error", err.Error()))
	}

	// A transcript change is the only thing that can move the conversation
	// title, so re-read it here (still on the worker) and republish.
	if p.transcript {
		d.refresh_title(ctx, e)
	}
}

// refresh_title reads claude's own conversation summary from the newest
// transcript and caches it on the entry for listings. It runs on the entry's
// worker (so writing e.item and publishing is race-free) and is best-effort:
// any failure — no transcript yet, a claude version that stopped writing the
// ai-title record, a docker hiccup — simply leaves the previous title in place.
func (d *Daemon) refresh_title(ctx context.Context, e *entry) {
	enc := claude.EncodeProjectPath(e.item.Workspace)
	dir := path.Join(e.cfg_dir, "projects", enc)
	// Take the last ai-title record of the most recently modified transcript.
	// Anchoring the match at the start of the line skips assistant messages
	// that merely quote the string "ai-title" in their body.
	script := `f=$(ls -t ` + tmuxx.Quote(dir) + `/*.jsonl 2>/dev/null | head -1); ` +
		`[ -n "$f" ] && grep '^{"type":"ai-title"' "$f" | tail -1`
	out, _, err := dockerx.ExecOutput(ctx, d.cli, e.id, e.user, []string{"sh", "-c", script})
	if err != nil {
		return
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return
	}
	var rec struct {
		Title string `json:"aiTitle"`
	}
	if json.Unmarshal([]byte(line), &rec) != nil || rec.Title == "" {
		return
	}
	if rec.Title == e.item.Title {
		return
	}
	e.item.Title = rec.Title
	e.publish()
}

// watch_container keeps an in-container watcher exec alive and feeds its
// change stream into the sync loop. The watcher is cld itself, copied into
// the container, streaming one changed path per line over the exec. If the
// watcher cannot run at all, it falls back to polling.
func (d *Daemon) watch_container(ctx context.Context, e *entry, id string) {
	fails := 0
	for ctx.Err() == nil {
		clean, err := d.watch_once(ctx, e, id)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			d.log.Warn("watcher error",
				slog.String("name", e.item.Name), slog.String("error", err.Error()))
		}

		insp, ierr := d.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if ierr != nil || insp.Container.State == nil || !insp.Container.State.Running {
			return
		}

		// A watcher that keeps exiting immediately (missing binary, exec
		// denied) is never going to work; fall back to polling instead of
		// re-execing forever.
		if clean {
			fails++
			if fails >= 3 {
				d.log.Warn("watcher unusable; falling back to polling",
					slog.String("name", e.item.Name))
				d.poll_container(ctx, e)
				return
			}
		} else {
			fails = 0
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// watch_once runs one watcher exec. clean reports that it exited on its own
// (as opposed to being cut off by a stream error), which distinguishes an
// unusable watcher from a transient disconnect.
func (d *Daemon) watch_once(ctx context.Context, e *entry, id string) (clean bool, err error) {
	created, err := d.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		User:         e.user,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          []string{path.Join(install_dir, "cld"), "x", "watch", e.cfg_dir},
	})
	if err != nil {
		return false, err
	}

	att, err := d.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return false, err
	}
	defer att.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			att.Close()
		case <-done:
		}
	}()

	// The non-TTY exec stream is multiplexed; demux stdout into a line pipe
	// and surface stderr for diagnostics.
	pr, pw := io.Pipe()
	var errbuf lineBuffer
	go func() {
		_, e := stdcopy.StdCopy(pw, &errbuf, att.Reader)
		pw.CloseWithError(e)
	}()

	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		switch claude.Classify(line) {
		case claude.BackupSettings:
			e.mark(dirty{settings: true})
		case claude.BackupTranscript:
			e.mark(dirty{transcript: true})
		}
	}
	serr := sc.Err()

	insp, ierr := d.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if ierr == nil && !insp.Running {
		// Exited on its own. Report a non-zero exit as the error so the
		// caller can log why the watcher is failing.
		if insp.ExitCode != 0 {
			return true, fmt.Errorf("watcher exited %d: %s", insp.ExitCode, errbuf.String())
		}
		return true, serr
	}
	if serr == nil && ctx.Err() == nil {
		serr = errors.New("watcher stream closed")
	}
	return false, serr
}

// poll_container is the fallback when the in-container watcher cannot run
// (container architecture differs from the host, or the watcher is unusable):
// periodic full snapshots.
func (d *Daemon) poll_container(ctx context.Context, e *entry) {
	t := time.NewTicker(d.cfg.Sync.FallbackInterval.Std())
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.mark(dirty{settings: true, transcript: true})
		}
	}
}
