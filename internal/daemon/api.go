package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// api serves the control plane: listing and session-exit notifications.
// No TTY ever flows through this socket.
func (d *Daemon) api() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": d.Items()})
	})

	mux.HandleFunc("POST /notify/exited", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("container")
		if id == "" {
			http.Error(w, "container required", http.StatusBadRequest)
			return
		}

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
			if !e.bind_mounted && e.item.Workspace != "" {
				d.copy_out(d.base_ctx, e, dirty{global: true, project: true})
			}
			e.item.Status = StatusSessionEnded
			e.session_done = false
			e.publish()
			d.log.Info("session exited", slog.String("name", e.item.Name))
		})
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
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

// NotifyExited tells the daemon a session's remote process ended.
func NotifyExited(ctx context.Context, socket string, container string) error {
	hc := NewSocketClient(socket)
	url := "http://cld/notify/exited?container=" + container
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
