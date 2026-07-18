// Package daemon implements "cld serve": it watches Docker events for
// devcontainers, provisions them with the claude binary, keeps one host-side
// tmux session per container, and syncs conversation state out to backups.
package daemon

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/broker"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/ghcli"
	"github.com/lesomnus/cld/internal/release"
	"github.com/lesomnus/cld/internal/tmuxx"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

type Status string

const (
	StatusProvisioning Status = "provisioning"
	StatusReady        Status = "ready"
	StatusStopped      Status = "stopped"
	StatusSessionEnded Status = "session-ended"
	StatusFailed       Status = "failed"
)

// Activity is what the claude conversation in a ready container is doing right
// now — a separate axis from Status (the container lifecycle). It is derived
// live from the session's tmux pane when listing, so it is empty for a
// container that is not ready.
type Activity string

const (
	// ActivityWorking: claude is generating (the pane shows its interrupt hint).
	ActivityWorking Activity = "working"
	// ActivityWaiting: claude has a conversation and is idle at the prompt.
	ActivityWaiting Activity = "waiting"
	// ActivityIdle: claude is up but no conversation has started yet.
	ActivityIdle Activity = "idle"
)

// Item is the externally visible state of one provisioned devcontainer.
type Item struct {
	ID string `json:"id"`
	// Name is the container's stable managed identity: the source of the tmux
	// session name (see devc.SessionName) used to find, attach to, and restore
	// the session, and the primary handle resolved by `cld it`. It is derived
	// from the full devcontainer.json "name" (or folder) and must stay stable
	// across upgrades — a namespaced name like "lesomnus/cld" stays
	// "lesomnus-cld" here so an already-running tmux session keeps matching.
	Name string `json:"name"`
	// Display is the user-facing label shown in `cld ls` and the tmux tab. It
	// collapses a namespaced name to its last segment ("lesomnus/cld" -> "cld")
	// for readability; unlike Name it carries no identity and is never used to
	// address a session, so changing how it reads never orphans a session.
	Display     string `json:"display,omitempty"`
	Alias       string `json:"alias"`
	LocalFolder string `json:"local_folder"`
	Workspace   string `json:"workspace"`
	Status      Status `json:"status"`
	Version     string `json:"version"`
	Error       string `json:"error,omitempty"`
	// Activity is the live conversation state; only set for ready containers
	// and filled in when listing (see withActivity), not by the worker.
	Activity Activity `json:"activity,omitempty"`
	// Title is claude's own summary of the conversation, cached from the
	// transcript by the worker (see refresh_title).
	Title string `json:"title,omitempty"`
	// StatusSince / ActivitySince are when Status / Activity last changed,
	// stamped by the daemon at the transition (see entry.publish) so a listing
	// can show how long a container has held its current state. Zero when the
	// transition was never observed by the daemon — notably Activity on a
	// poll-only (cross-arch / no-relay) container, which is reclassified from
	// the pane on every listing and never stored, so there is no moment to mark.
	StatusSince   time.Time `json:"status_since,omitzero"`
	ActivitySince time.Time `json:"activity_since,omitzero"`
	// Workflows are the Claude Code multi-agent workflow runs of the session's
	// current transcript, derived by the daemon from the on-disk run journals
	// (see refresh_workflows). Empty when the session has run none. Ordered
	// newest-first by UpdatedAt.
	Workflows []WorkflowRun `json:"workflows,omitempty"`
}

// WorkflowRun is the observable state of one Claude Code workflow run, read
// from two on-disk sources with very different reliability:
//
//   - Progress (Total/Done) comes from the run journal,
//     subagents/workflows/<run_id>/journal.jsonl, which appends one "started"
//     line per fanned-out agent and one "result" line when that agent returns.
//     The journal has NO timestamps and its format is internal/versioned ("v2:"
//     keys), so it is parsed best-effort and only counted, never trusted for
//     structure. It is the only source that exists while a run is still live.
//
//   - Finalization (Finalized/Status) comes from the run's state file,
//     workflows/<run_id>.json, which Claude Code writes only when the run ENDS.
//     Its presence is therefore an authoritative "no longer live" signal. Status
//     is that file's own status word, read best-effort (the file is one big JSON
//     line whose embedded script/results could hold look-alikes), so it is
//     treated as advisory: only a recognized failure word ever changes how a
//     finalized run is shown, and a misread degrades to "completed".
type WorkflowRun struct {
	RunID string `json:"run_id"`         // e.g. "wf_c310b23a-0d6"
	Name  string `json:"name,omitempty"` // workflow meta.name, from the persisted script filename
	Total int    `json:"total"`          // agents started (journal)
	Done  int    `json:"done"`           // agents that returned a result (journal)
	// Finalized is set once the run wrote its state file, i.e. it is no longer
	// live. Status is that file's own status word ("completed", "failed", …),
	// advisory and empty while a run is live or if it could not be read.
	Finalized bool   `json:"finalized,omitempty"`
	Status    string `json:"status,omitempty"`
	// UpdatedAt is the freshest write time across the run's journal and its
	// per-agent transcripts — the liveness signal for a run that has not
	// finalized. The journal alone lags (it advances only on agent start/return,
	// so a long single agent leaves it quiet), so a run's agent files count too.
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// Running is the number of a run's agents that started but have not yet
// returned a result.
func (w WorkflowRun) Running() int {
	if n := w.Total - w.Done; n > 0 {
		return n
	}
	return 0
}

// entry is one container's state. Every mutable field except the published
// snapshot is owned by the container's single worker goroutine (see mailbox),
// so provisioning code needs no locks. Items() reads the atomic snapshot.
type entry struct {
	id   string
	mbox *mailbox

	item Item                 // worker-owned canonical state
	snap atomic.Pointer[Item] // published copy for Items()

	user       string
	uid        int
	gid        int
	home       string
	cache_home string // $XDG_CACHE_HOME or $HOME/.cache; parent of the relay socket
	cfg_dir    string
	dev_name   string // devcontainer.json "name", or "" if unset

	platform release.Platform
	arch     string // container arch reported by the image ("amd64"/"arm64"); for gh
	arch_ok  bool   // container arch == host arch; self-copy and watcher possible

	// activity_pushed means claude's in-container hooks can report conversation
	// activity over the scoped relay (arch match + remote control), so the worker
	// owns e.item.Activity and the listing trusts the snapshot rather than
	// capturing the pane. Set at ready in ensure_; read only there and by pushes.
	activity_pushed bool

	status_since   time.Time // when item.Status last changed; stamped in publish
	activity_since time.Time // when item.Activity last changed (push path only)

	restored       bool
	session_done   bool   // session was evaluated for the current start generation
	session_failed bool   // this generation's session exited non-zero; keep it visible
	git_config     bool   // host gitconfig was installed into the config dir
	started_at     string // container State.StartedAt of the current generation
	version        string

	watch_stop context.CancelFunc // cancels the watcher and sync goroutines

	dirty_mu  sync.Mutex
	dirty     dirty
	dirty_sig chan struct{} // capacity 1; coalescing wakeup for sync_loop
}

func (e *entry) publish() {
	// Stamp the moment Status or Activity takes its current value so a listing
	// can show how long the container has held it. publish() also fires for
	// unrelated changes (title refresh, version, error), so only an actual
	// change to the field moves its mark. The poll-only activity path never
	// reaches here — it is classified on the returned listing copy, not stored
	// and never published — so activity_since stays zero for those containers
	// and the listing honestly shows no duration.
	now := time.Now()
	if prev := e.snap.Load(); prev == nil {
		e.status_since = now
		e.activity_since = now
	} else {
		if prev.Status != e.item.Status {
			e.status_since = now
		}
		if prev.Activity != e.item.Activity {
			e.activity_since = now
		}
	}
	e.item.StatusSince = e.status_since
	e.item.ActivitySince = e.activity_since

	it := e.item
	e.snap.Store(&it)
}

func (e *entry) snapshot() Item {
	if p := e.snap.Load(); p != nil {
		return *p
	}
	return Item{ID: e.id}
}

// dirty accumulates which parts of a container's config dir changed since
// the last copy-out. Both kinds land in the SAME isolated per-project backup
// dir, keyed by backup_key (see copy_out) — never a bucket shared across
// projects — so settings/skills/etc. changed inside one project's container
// can only ever affect that project's own backup. The two fields are tracked
// separately purely to avoid re-fetching a (potentially huge) transcript tree
// just because settings.json changed, and vice versa.
type dirty struct {
	settings   bool
	transcript bool
}

type Daemon struct {
	cfg  *config.Config
	cli  *client.Client
	tmux *tmuxx.Server
	rel  *release.Manager
	gh   *ghcli.Fetcher
	log  *slog.Logger

	self     string // path of the cld executable, reused as pane client and watcher
	self_ctr string // container ID when the daemon itself runs in one, else ""
	sessions *sessionStore
	proxy    *proxyStore    // per-project opt-in to broker-proxy auth (see proxyStore)
	broker   *broker.Broker // central subscription-auth broker (see internal/broker)

	base_ctx context.Context // long-lived; parents watcher/sync goroutines
	wg       sync.WaitGroup  // tracks worker goroutines

	// proj_locks serializes access to a project backup dir, which containers
	// that share a backup key (same devcontainer name) would otherwise write
	// concurrently.
	proj_locks keyedLock

	mu      sync.Mutex
	entries map[string]*entry
}

func New(cfg *config.Config, cli *client.Client, log *slog.Logger) (*Daemon, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}

	rc := release.NewClient(cfg.Release.BaseURL)
	return &Daemon{
		cfg:  cfg,
		cli:  cli,
		tmux: &tmuxx.Server{Socket: cfg.TmuxSocketPath()},
		rel: &release.Manager{
			Client:  rc,
			Cache:   &release.Cache{Dir: cfg.BinDir(), Client: rc},
			Channel: cfg.Release.Channel,
			Log:     log,
		},
		gh:       &ghcli.Fetcher{Dir: cfg.GhBinDir()},
		log:      log,
		self:     self,
		sessions: &sessionStore{dir: filepath.Join(cfg.CacheDir, "sessions")},
		proxy:    &proxyStore{dir: cfg.ProxyStateDir()},
		broker:   broker.New(broker.FileStore{Path: cfg.BrokerCredentialsPath()}),
		entries:  map[string]*entry{},
	}, nil
}

func (d *Daemon) Run(ctx context.Context) error {
	d.base_ctx = ctx
	d.self_ctr = detect_self_container(ctx, d.cli)
	if d.self_ctr != "" {
		d.log.Info("running inside a container", slog.String("id", short(d.self_ctr)))
	}

	if err := os.MkdirAll(d.cfg.CacheDir, 0o755); err != nil {
		return err
	}

	ln, err := d.listen()
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(d.cfg.SocketPath())

	server := &http.Server{Handler: d.api()}
	go server.Serve(ln)
	defer server.Close()

	go d.rel.RefreshLoop(ctx, d.cfg.Release.CheckInterval.Std())

	d.log.Info("serving", slog.String("socket", d.cfg.SocketPath()))
	err = d.watch_events(ctx)

	// Drain in-flight worker tasks (a final copy-out may be running) before
	// returning, so process exit does not truncate a backup write.
	d.shutdown_workers()
	return err
}

// shutdown_workers closes every mailbox and waits for the workers to drain.
func (d *Daemon) shutdown_workers() {
	d.mu.Lock()
	boxes := make([]*mailbox, 0, len(d.entries))
	for _, e := range d.entries {
		boxes = append(boxes, e.mbox)
	}
	d.mu.Unlock()

	for _, b := range boxes {
		b.close()
	}
	d.wg.Wait()
}

// listen binds the API socket, replacing a stale socket file but refusing
// to start when another daemon is alive on it.
func (d *Daemon) listen() (net.Listener, error) {
	p := d.cfg.SocketPath()
	if _, err := os.Stat(p); err == nil {
		conn, err := net.DialTimeout("unix", p, time.Second)
		if err == nil {
			conn.Close()
			return nil, os.ErrExist
		}
		os.Remove(p)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	return net.Listen("unix", p)
}

// watch_events subscribes to the event stream first and reconciles after,
// so containers started in between are never missed. On stream errors it
// resubscribes and reconciles again.
func (d *Daemon) watch_events(ctx context.Context) error {
	for {
		res := d.cli.Events(ctx, client.EventsListOptions{
			Filters: client.Filters{
				"type":  {string(events.ContainerEventType): true},
				"event": {"start": true, "die": true, "destroy": true},
			},
		})

		d.reconcile(ctx)

	stream:
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()

			case msg := <-res.Messages:
				d.handle_event(ctx, msg)

			case err := <-res.Err:
				if ctx.Err() != nil {
					return ctx.Err()
				}
				d.log.Warn("event stream broken; resubscribing", slog.String("error", err.Error()))
				break stream
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// handle_event posts work to the container's serialized worker. All posts
// come from this single goroutine (and reconcile, also on this goroutine),
// so per-container ordering matches the event stream.
func (d *Daemon) handle_event(ctx context.Context, msg events.Message) {
	id := msg.Actor.ID
	switch msg.Action {
	case "start":
		e := d.get_or_create(id)
		e.mbox.post(func() { d.ensure(ctx, e) })
	case "die":
		if e := d.lookup(id); e != nil {
			e.mbox.post(func() { d.stop(ctx, e) })
		}
	case "destroy":
		if e := d.lookup(id); e != nil {
			e.mbox.post(func() { d.teardown(ctx, e) })
		}
	}
}

// reconcile lists every devcontainer, running or stopped, and ensures each;
// entries whose container is gone (destroyed, not merely stopped) are torn
// down. Stopped containers are listed with All so a daemon that starts while a
// container is down still shows it, and so a container that stops mid-run is
// kept in the listing rather than dropped as if it had vanished.
func (d *Daemon) reconcile(ctx context.Context) {
	res, err := d.cli.ContainerList(ctx, client.ContainerListOptions{
		All: true,
		Filters: client.Filters{
			"label": {devc.LabelLocalFolder: true},
		},
	})
	if err != nil {
		d.log.Warn("container list failed", slog.String("error", err.Error()))
		return
	}

	alive := map[string]bool{}
	for _, c := range res.Items {
		alive[c.ID] = true
		e := d.get_or_create(c.ID)
		e.mbox.post(func() { d.ensure(ctx, e) })
	}

	d.mu.Lock()
	stale := make([]*entry, 0)
	for id, e := range d.entries {
		if !alive[id] {
			stale = append(stale, e)
		}
	}
	d.mu.Unlock()
	for _, e := range stale {
		e.mbox.post(func() { d.teardown(ctx, e) })
	}
}

func (d *Daemon) lookup(id string) *entry {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.entries[id]
}

// is_tracked reports whether e is still the daemon's current entry for its id.
// It fails once d.remove(e) has dropped it — e.g. after ensure decides the
// container is ignored, or a destroy retired it — even though a task posted
// earlier still holds the pointer. down --all uses it as a cheap early-out for
// an already-retired entry; the authoritative scope check is a live re-inspect
// (managed_devcontainer), since a still-tracked entry may just not have been
// classified yet.
func (d *Daemon) is_tracked(e *entry) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.entries[e.id] == e
}

// get_or_create returns the entry for id, starting its worker on first sight.
func (d *Daemon) get_or_create(id string) *entry {
	d.mu.Lock()
	defer d.mu.Unlock()

	if e, ok := d.entries[id]; ok {
		return e
	}
	e := &entry{
		id:        id,
		mbox:      new_mailbox(),
		dirty_sig: make(chan struct{}, 1),
	}
	e.item.ID = id
	e.publish()
	d.entries[id] = e

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		e.mbox.run()
	}()
	return e
}

// remove drops an entry and stops its worker after the current task returns.
func (d *Daemon) remove(e *entry) {
	d.mu.Lock()
	if d.entries[e.id] == e {
		delete(d.entries, e.id)
	}
	d.mu.Unlock()
	e.mbox.close()
}

// Items snapshots the current listing, sorted by name.
func (d *Daemon) Items() []Item {
	d.mu.Lock()
	entries := make([]*entry, 0, len(d.entries))
	for _, e := range d.entries {
		entries = append(entries, e)
	}
	d.mu.Unlock()

	items := make([]Item, 0, len(entries))
	for _, e := range entries {
		items = append(items, e.snapshot())
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items
}

// withActivity fills the live conversation Activity of every ready item by
// reading its tmux pane. It works off the published snapshots (Name, Status,
// Title) — never entry-owned fields — so it is safe to call from a request
// handler. A pane that cannot be read leaves the item classified as waiting
// rather than failing the whole listing.
func (d *Daemon) withActivity(ctx context.Context, items []Item) []Item {
	d.fillActivity(ctx, items, nil)
	return items
}

// fillActivity resolves each ready item's Activity. A push-capable container
// (arch match + remote control) keeps its Activity current in the snapshot via
// claude's in-container hooks, so a non-empty Activity is trusted as-is and the
// pane is not captured. Only containers that cannot push (cross-arch, no relay)
// fall back to classifying the captured tmux pane. When panes is non-nil (the
// `?debug` listing), the pane IS captured even for push containers — for
// comparison — but a pushed Activity is never overwritten.
func (d *Daemon) fillActivity(ctx context.Context, items []Item, panes map[string]string) {
	for i := range items {
		if items[i].Status != StatusReady {
			continue
		}
		pushed := items[i].Activity != ""
		if pushed && panes == nil {
			continue
		}
		pane, _ := d.tmux.CapturePane(ctx, devc.SessionName(items[i].Name))
		if panes != nil {
			panes[items[i].ID] = pane
		}
		if !pushed {
			items[i].Activity = classifyActivity(pane, items[i].Title)
		}
	}
}

// paneWorkingHint is the substring Claude Code's TUI shows in its footer while
// it is generating ("esc to interrupt"). Matching the TUI is brittle across
// claude versions, so a miss only misclassifies a working pane as waiting — it
// never breaks the listing.
const paneWorkingHint = "to interrupt"

// classifyActivity turns a captured pane and the cached title into a
// conversation Activity. A pane still showing the interrupt hint is working;
// otherwise the container is idle when no conversation has produced a title yet
// and waiting once it has.
func classifyActivity(pane, title string) Activity {
	if strings.Contains(pane, paneWorkingHint) {
		return ActivityWorking
	}
	if title == "" {
		return ActivityIdle
	}
	return ActivityWaiting
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
