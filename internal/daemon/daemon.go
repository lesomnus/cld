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
	ID          string `json:"id"`
	Name        string `json:"name"`
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
	arch_ok  bool // container arch == host arch; self-copy and watcher possible

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
	for i := range items {
		if items[i].Status != StatusReady {
			continue
		}
		pane, _ := d.tmux.CapturePane(ctx, devc.SessionName(items[i].Name))
		items[i].Activity = classifyActivity(pane, items[i].Title)
	}
	return items
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
