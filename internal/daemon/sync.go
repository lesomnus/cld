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
	"sort"
	"strconv"
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
	// title or the workflow-run state, so re-read both here (still on the
	// worker) and republish. Workflow journals live under projects/ too, so the
	// same transcript-dirty signal already covers their writes.
	if p.transcript {
		d.refresh_title(ctx, e)
		d.refresh_workflows(ctx, e)
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

// refresh_workflows reads the state of the session's Claude Code workflow runs
// from their on-disk journals and caches it on the entry for listings. Like
// refresh_title it runs on the worker (race-free publish) and is best-effort:
// the run-journal format is internal to Claude Code, so any parse miss just
// leaves the previous value in place rather than dropping the whole listing.
//
// One shell pass emits a tab-separated line per run —
// "<run_id> <started> <result> <mtime> <name>" — for the current session (the
// newest transcript, the same one refresh_title reads). Runs live under
// <session>/subagents/workflows/wf_*/, and each run's script, persisted as
// "<meta.name>-<run_id>.js" under <session>/workflows/scripts, is where the
// human-readable name comes from. Counting is a plain grep: the journal writes
// one {"type":"started"} per fanned-out agent and one {"type":"result"} when it
// returns, with no run-level record.
func (d *Daemon) refresh_workflows(ctx context.Context, e *entry) {
	enc := claude.EncodeProjectPath(e.item.Workspace)
	dir := path.Join(e.cfg_dir, "projects", enc)
	// Per run, emit a tab-separated line:
	//   run_id  started  result  updated_mtime  finalized  status  name
	//
	//   - started/result: line counts of the journal's own record types. The
	//     match is anchored at "^{" so a "type":"started" appearing INSIDE a
	//     result record's nested payload (a real case when a workflow's agents
	//     discuss journal formats) is not miscounted — the record's own type is
	//     always the first key. Mirrors refresh_title's anchored ai-title grep.
	//   - updated_mtime: the freshest of the journal's mtime and the newest
	//     per-agent transcript's mtime. The journal lags (it only moves on agent
	//     start/return), so a run with one long agent would look stale from the
	//     journal alone while its agent file is actively written.
	//   - finalized: whether the run wrote its state file — an authoritative
	//     "no longer live" signal, taken from the file's mere existence.
	//   - status: the state file's status word, best-effort via grep -o (no jq
	//     in the container). Advisory only, so a wrong grab cannot mislabel a
	//     live run — the reader treats an unrecognized value as "completed".
	//   - name: the run's meta.name, recovered from its persisted script's
	//     filename "<name>-<run_id>.js" (reliable; emitted last so a name with
	//     spaces cannot break the column split).
	script := `d=` + tmuxx.Quote(dir) + `; ` +
		`s=$(ls -t "$d"/*.jsonl 2>/dev/null | head -1); [ -n "$s" ] || exit 0; ` +
		`s=$(basename "$s" .jsonl); wd="$d/$s/workflows"; ` +
		`for j in "$d/$s/subagents/workflows"/wf_*/journal.jsonl; do ` +
		`[ -f "$j" ] || continue; ` +
		`dir=$(dirname "$j"); r=$(basename "$dir"); ` +
		`st=$(grep -c '^{"type":"started"' "$j"); ` +
		`re=$(grep -c '^{"type":"result"' "$j"); ` +
		`mt=$(stat -c %Y "$j" 2>/dev/null || echo 0); ` +
		`am=$(ls -t "$dir"/agent-*.jsonl 2>/dev/null | head -1); ` +
		`if [ -n "$am" ]; then amt=$(stat -c %Y "$am" 2>/dev/null || echo 0); [ "$amt" -gt "$mt" ] && mt=$amt; fi; ` +
		`sf="$wd/$r.json"; fin=0; status=""; ` +
		`if [ -f "$sf" ]; then fin=1; ` +
		`status=$(grep -o '"status":"[^"]*"' "$sf" | head -1 | sed 's/.*:"//;s/"$//'); fi; ` +
		`nm=""; for f in "$wd/scripts/"*-"$r".js; do ` +
		`[ -f "$f" ] && nm=$(basename "$f" .js) && nm=${nm%-"$r"} && break; done; ` +
		`printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$r" "$st" "$re" "$mt" "$fin" "$status" "$nm"; ` +
		`done`
	out, _, err := dockerx.ExecOutput(ctx, d.cli, e.id, e.user, []string{"sh", "-c", script})
	if err != nil {
		return
	}
	runs := parseWorkflowRuns(out)
	if workflowRunsEqual(e.item.Workflows, runs) {
		return
	}
	e.item.Workflows = runs
	e.publish()
}

// parseWorkflowRuns turns refresh_workflows' tab-separated output into runs,
// ordered newest-first. A malformed line is skipped, never fatal. Fields after
// the first four are optional (a live run has no state file), so a short line
// still yields a valid run with just its progress counts.
func parseWorkflowRuns(out string) []WorkflowRun {
	var runs []WorkflowRun
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 4 || f[0] == "" {
			continue
		}
		total, err1 := strconv.Atoi(strings.TrimSpace(f[1]))
		done, err2 := strconv.Atoi(strings.TrimSpace(f[2]))
		if err1 != nil || err2 != nil {
			continue
		}
		run := WorkflowRun{RunID: f[0], Total: total, Done: done}
		if mt, err := strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64); err == nil && mt > 0 {
			run.UpdatedAt = time.Unix(mt, 0)
		}
		if len(f) >= 5 {
			run.Finalized = strings.TrimSpace(f[4]) == "1"
		}
		if len(f) >= 6 {
			run.Status = strings.TrimSpace(f[5])
		}
		if len(f) >= 7 {
			run.Name = strings.TrimSpace(f[6])
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].UpdatedAt.Equal(runs[j].UpdatedAt) {
			return runs[i].UpdatedAt.After(runs[j].UpdatedAt)
		}
		return runs[i].RunID < runs[j].RunID
	})
	return runs
}

// workflowRunsEqual reports whether two run slices are identical, used to skip a
// republish when nothing changed. Time fields are compared with Equal rather
// than == so the check never depends on a monotonic/location quirk.
func workflowRunsEqual(a, b []WorkflowRun) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].RunID != b[i].RunID || a[i].Name != b[i].Name ||
			a[i].Total != b[i].Total || a[i].Done != b[i].Done ||
			a[i].Finalized != b[i].Finalized || a[i].Status != b[i].Status ||
			!a[i].UpdatedAt.Equal(b[i].UpdatedAt) {
			return false
		}
	}
	return true
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
