package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	urlpkg "net/url"
	"os"
	"strconv"
	"time"
)

// Info tells clients where the daemon — and so the tmux server — lives, so
// `cld it` can attach through a `docker exec` when the daemon is in a
// container instead of requiring a local tmux.
type Info struct {
	// ContainerID is set when the daemon runs inside a container.
	ContainerID string `json:"container_id,omitempty"`
	// TmuxSocket is the tmux server socket path as seen by the daemon.
	TmuxSocket string `json:"tmux_socket"`
	// UID the daemon runs as; the attach exec must match it for tmux to
	// accept the client.
	UID int `json:"uid"`
	// APIAttach reports that the daemon can stream a tmux attach over this
	// control socket (GET /session/attach). It lets a client reaching the
	// daemon through the in-container relay attach with no docker or tmux of
	// its own. Only offered when the daemon runs in a container.
	APIAttach bool `json:"api_attach,omitempty"`
}

// api serves the full control plane on the daemon's own socket, for trusted
// host-side clients. No TTY flows here except the hijacked GET /session/attach.
func (d *Daemon) api() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items", d.handle_items)
	mux.HandleFunc("GET /info", d.handle_info)
	mux.HandleFunc("GET /session/attach", d.handle_attach)
	mux.HandleFunc("POST /notify/exited", d.handle_notify_exited)
	mux.HandleFunc("POST /session/new", d.handle_session_new)
	mux.HandleFunc("POST /down", d.handle_down)
	return mux
}

// scoped_api is the control plane exposed to ONE container through the
// in-container relay. Every operation is confined to that container's own
// session: it may list and attach to itself, and recreate or down itself, but
// can neither see nor act on any other project. This keeps the relay from being
// a cross-container lateral path when a managed container runs untrusted code.
// The identity is bound here (self_id), not supplied by the caller, so a
// container cannot address another.
func (d *Daemon) scoped_api(self_id string) http.Handler {
	self_name := func() string {
		if e := d.lookup(self_id); e != nil {
			return e.snapshot().Name
		}
		return ""
	}
	// only_self rejects a request whose ?name= is not this container's own.
	only_self := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if n := self_name(); n == "" || r.URL.Query().Get("name") != n {
				http.Error(w, "forbidden: not your session", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", d.handle_info)
	mux.HandleFunc("GET /items", func(w http.ResponseWriter, r *http.Request) {
		mine := make([]Item, 0, 1)
		for _, it := range d.Items() {
			if it.ID == self_id {
				mine = append(mine, it)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": mine})
	})
	mux.HandleFunc("GET /session/attach", only_self(d.handle_attach))
	mux.HandleFunc("POST /session/new", only_self(d.handle_session_new))
	mux.HandleFunc("POST /down", only_self(d.handle_down))
	mux.HandleFunc("POST /notify/exited", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("container") != self_id {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		d.handle_notify_exited(w, r)
	})
	return mux
}

func (d *Daemon) handle_items(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"items": d.Items()})
}

func (d *Daemon) handle_info(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Info{
		ContainerID: d.self_ctr,
		TmuxSocket:  d.cfg.TmuxSocketPath(),
		UID:         os.Getuid(),
		APIAttach:   d.self_ctr != "",
	})
}

func (d *Daemon) handle_notify_exited(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("container")
	if id == "" {
		http.Error(w, "container required", http.StatusBadRequest)
		return
	}

	gen := r.URL.Query().Get("gen")
	code, _ := strconv.Atoi(r.URL.Query().Get("code"))

	// Look up only; a stale notify for an unknown container must not create a
	// phantom entry in the listing.
	e := d.lookup(id)
	if e == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	e.mbox.post(func() {
		if e.item.Name == "" || e.item.Status == StatusStopped {
			return
		}
		// Ignore a notify from a superseded generation: the container has
		// since restarted and a fresh session exists.
		if gen != "" && gen != e.started_at {
			return
		}
		if e.item.Workspace != "" {
			d.copy_out(d.base_ctx, e, dirty{global: true, project: true})
		}
		if code != 0 {
			// A non-zero exit is a crash or a failed launch, not the user
			// quitting: surface it as failed instead of masking it as a clean
			// end. session_failed keeps it settled so a reconcile does not
			// silently flip it back to ready; `cld it --new` retries.
			e.session_failed = true
			e.item.Status = StatusFailed
			e.item.Error = fmt.Sprintf("session exited with status %d", code)
			e.publish()
			d.log.Warn("session failed",
				slog.String("name", e.item.Name), slog.Int("code", code))
			return
		}
		// A clean exit is the user ending the session. Persist it so a daemon
		// restart does not resurrect it.
		e.session_failed = false
		d.sessions.set(id, sessionState{Gen: e.started_at, Ended: true})
		e.item.Status = StatusSessionEnded
		e.item.Error = ""
		e.publish()
		d.log.Info("session exited", slog.String("name", e.item.Name))
	})
	w.WriteHeader(http.StatusNoContent)
}

// handle_session_new recreates a session the user ended, by display name. Backs
// `cld it --new`.
func (d *Daemon) handle_session_new(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	e := d.by_name(name)
	if e == nil {
		http.Error(w, "no such devcontainer", http.StatusNotFound)
		return
	}

	done := make(chan error, 1)
	// If the container was torn down between lookup and post, the mailbox is
	// closed and the task would never run; don't wait on it.
	if !e.mbox.post(func() { done <- d.recreate_session(d.base_ctx, e) }) {
		http.Error(w, "container is no longer tracked", http.StatusConflict)
		return
	}
	if err := <-done; err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handle_down stops and removes a devcontainer, by display name. Backs
// `cld down`. The final backup and removal run on the container's worker so the
// copy-out finishes before Docker drops the container.
func (d *Daemon) handle_down(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	e := d.by_name(name)
	if e == nil {
		http.Error(w, "no such devcontainer", http.StatusNotFound)
		return
	}

	done := make(chan error, 1)
	if !e.mbox.post(func() { done <- d.down(d.base_ctx, e) }) {
		http.Error(w, "container is no longer tracked", http.StatusConflict)
		return
	}
	if err := <-done; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// by_name finds a tracked entry by its display name.
func (d *Daemon) by_name(name string) *entry {
	d.mu.Lock()
	entries := make([]*entry, 0, len(d.entries))
	for _, e := range d.entries {
		entries = append(entries, e)
	}
	d.mu.Unlock()

	for _, e := range entries {
		if e.snapshot().Name == name {
			return e
		}
	}
	return nil
}

// NewSocketClient returns an HTTP client that dials the daemon's unix socket.
func NewSocketClient(socket string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
	}
}

// FetchItems asks a running daemon for its listing.
func FetchItems(ctx context.Context, socket string) ([]Item, error) {
	hc := NewSocketClient(socket)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cld/items", nil)
	if err != nil {
		return nil, err
	}

	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("is `cld serve` running? %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon: %s", res.Status)
	}

	var body struct {
		Items []Item `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Items, nil
}

// FetchInfo asks a running daemon where it (and its tmux server) lives.
func FetchInfo(ctx context.Context, socket string) (*Info, error) {
	hc := NewSocketClient(socket)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cld/info", nil)
	if err != nil {
		return nil, err
	}

	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("is `cld serve` running? %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon: %s", res.Status)
	}

	var info Info
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		return nil, err
	}
	return &info, nil
}

// NotifyExited tells the daemon a session's remote process ended. gen is the
// generation the session was launched for, so the daemon can ignore a stale
// notify from a previous container generation. code is the process exit status:
// 0 means the user ended the session, non-zero means it failed.
func NotifyExited(ctx context.Context, socket string, container string, gen string, code int) error {
	hc := NewSocketClient(socket)
	url := "http://cld/notify/exited?container=" + urlpkg.QueryEscape(container) +
		"&gen=" + urlpkg.QueryEscape(gen) + "&code=" + strconv.Itoa(code)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, res.Body)
	return res.Body.Close()
}

// RecreateSession asks the daemon to recreate a devcontainer's session.
func RecreateSession(ctx context.Context, socket string, name string) error {
	hc := NewSocketClient(socket)
	url := "http://cld/session/new?name=" + urlpkg.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	res, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("is `cld serve` running? %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("daemon: %s: %s", res.Status, string(body))
	}
	return nil
}

// Down asks the daemon to stop and remove a devcontainer. The daemon takes a
// final backup first, so the conversation history survives the removal. It uses
// a longer timeout than the other calls because that backup plus tearing down a
// Compose project can take a while.
func Down(ctx context.Context, socket string, name string) error {
	hc := NewSocketClient(socket)
	hc.Timeout = 2 * time.Minute
	url := "http://cld/down?name=" + urlpkg.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	res, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("is `cld serve` running? %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("daemon: %s: %s", res.Status, string(body))
	}
	return nil
}
