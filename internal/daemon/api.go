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
}

// api serves the control plane: listing and session-exit notifications.
// No TTY ever flows through this socket.
func (d *Daemon) api() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": d.Items()})
	})

	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Info{
			ContainerID: d.self_ctr,
			TmuxSocket:  d.cfg.TmuxSocketPath(),
			UID:         os.Getuid(),
		})
	})

	mux.HandleFunc("POST /notify/exited", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("container")
		if id == "" {
			http.Error(w, "container required", http.StatusBadRequest)
			return
		}

		gen := r.URL.Query().Get("gen")

		// Look up only; a stale notify for an unknown container must not
		// create a phantom entry in the listing.
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
			// Persist that the user ended this generation's session so a
			// daemon restart does not resurrect it.
			d.sessions.set(id, sessionState{Gen: e.started_at, Ended: true})
			e.item.Status = StatusSessionEnded
			e.publish()
			d.log.Info("session exited", slog.String("name", e.item.Name))
		})
		w.WriteHeader(http.StatusNoContent)
	})

	// Recreate a session the user ended, addressed by display name. Backs
	// `cld it --new`.
	mux.HandleFunc("POST /session/new", func(w http.ResponseWriter, r *http.Request) {
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
		// If the container was torn down between lookup and post, the mailbox
		// is closed and the task would never run; don't wait on it.
		if !e.mbox.post(func() { done <- d.recreate_session(d.base_ctx, e) }) {
			http.Error(w, "container is no longer tracked", http.StatusConflict)
			return
		}
		if err := <-done; err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
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
// notify from a previous container generation.
func NotifyExited(ctx context.Context, socket string, container string, gen string) error {
	hc := NewSocketClient(socket)
	url := "http://cld/notify/exited?container=" + urlpkg.QueryEscape(container) +
		"&gen=" + urlpkg.QueryEscape(gen)
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
