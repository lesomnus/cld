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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lesomnus/cld/internal/broker"
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
	mux.HandleFunc("POST /down/all", d.handle_down_all)
	mux.HandleFunc("POST /purge", d.handle_purge)
	mux.HandleFunc("POST /purge/all", d.handle_purge_all)
	mux.HandleFunc("POST /auth/credentials", d.handle_set_credentials)
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
		json.NewEncoder(w).Encode(map[string]any{"items": d.withActivity(r.Context(), mine)})
	})
	mux.HandleFunc("GET /session/attach", only_self(d.handle_attach))
	mux.HandleFunc("POST /session/new", only_self(d.handle_session_new))
	mux.HandleFunc("POST /down", only_self(d.handle_down))
	// A container reports its OWN conversation activity here (claude's hooks call
	// `cld x activity <state>`). The identity is the bound self_id, not a caller
	// argument, so it is inherently self-scoped — a container can only ever set
	// its own activity — and needs no ?name= / only_self guard.
	mux.HandleFunc("POST /activity", func(w http.ResponseWriter, r *http.Request) {
		d.handle_activity(w, r, self_id)
	})
	mux.HandleFunc("POST /notify/exited", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("container") != self_id {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		d.handle_notify_exited(w, r)
	})
	// The broker login is deliberately reachable from a container: it is how
	// `cld auth login` works from inside a devcontainer where the user's shell
	// lives. Unlike the other scoped routes this is NOT self-scoped — the broker
	// login is global — so any container that can reach the relay can replace it.
	// That is the same trust boundary as remote_control itself (which gates this
	// relay's existence); set remote_control=false to close it entirely.
	mux.HandleFunc("POST /auth/credentials", d.handle_set_credentials)
	return mux
}

func (d *Daemon) handle_items(w http.ResponseWriter, r *http.Request) {
	items := d.Items()

	// `?debug` also returns the raw captured pane per item, so `cld ls
	// --debug-activity` can show exactly what the working/waiting classifier
	// saw — the pane string is the only input to that decision.
	var panes map[string]string
	if r.URL.Query().Has("debug") {
		panes = make(map[string]string, len(items))
	}
	d.fillActivity(r.Context(), items, panes)

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"items": items}
	if panes != nil {
		resp["panes"] = panes
	}
	json.NewEncoder(w).Encode(resp)
}

// handle_activity records a container's self-reported conversation activity,
// pushed by claude's in-container hooks. self_id is bound by the scoped relay,
// so the state only ever applies to the caller's own session. The write goes
// through the entry's worker mailbox (like handle_notify_exited) so it never
// races the worker's own e.item mutations, and republishes only on a change.
func (d *Daemon) handle_activity(w http.ResponseWriter, r *http.Request, self_id string) {
	state := Activity(r.URL.Query().Get("state"))
	switch state {
	case ActivityWorking, ActivityWaiting, ActivityIdle:
	default:
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}

	e := d.lookup(self_id)
	if e == nil {
		http.Error(w, "session is not tracked", http.StatusNotFound)
		return
	}
	e.mbox.post(func() {
		if e.item.Status == StatusReady && e.item.Activity != state {
			e.item.Activity = state
			e.publish()
		}
	})
	w.WriteHeader(http.StatusNoContent)
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
			d.copy_out(d.base_ctx, e, dirty{settings: true, transcript: true})
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
// `cld it --new`. An optional ?proxy=on|off first records the project's
// proxy-auth preference (backing `--proxy`/`--no-proxy`), so the recreated
// session reflects the new mode; an absent/other value leaves it unchanged.
func (d *Daemon) handle_session_new(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	proxy := r.URL.Query().Get("proxy") // "", "on", or "off"
	e := d.by_name(name)
	if e == nil {
		http.Error(w, "no such devcontainer", http.StatusNotFound)
		return
	}

	done := make(chan error, 1)
	// If the container was torn down between lookup and post, the mailbox is
	// closed and the task would never run; don't wait on it. The proxy
	// preference is set on the worker too, where backup_key's inputs are stable.
	if !e.mbox.post(func() {
		if proxy == "on" || proxy == "off" {
			if err := d.proxy.set(d.backup_key(e), proxy == "on"); err != nil {
				done <- err
				return
			}
		}
		done <- d.recreate_session(d.base_ctx, e)
	}) {
		http.Error(w, "container is no longer tracked", http.StatusConflict)
		return
	}
	if err := <-done; err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handle_down stops and removes a devcontainer, by display name, keeping its
// volumes and backup. Backs `cld down`.
func (d *Daemon) handle_down(w http.ResponseWriter, r *http.Request) {
	d.handle_teardown(w, r, false)
}

// handle_purge stops and removes a devcontainer, by display name, and deletes
// its named volumes and host-side conversation backup. Backs `cld purge`. It is
// only on the full control plane, never the in-container scoped_api — a managed
// container must not be able to erase its own (or any) history.
func (d *Daemon) handle_purge(w http.ResponseWriter, r *http.Request) {
	d.handle_teardown(w, r, true)
}

// handle_teardown backs both `cld down` and `cld purge`: the final backup (down
// only) and removal run on the container's worker so the copy-out finishes
// before Docker drops the container. purge additionally deletes the volumes and
// backup.
func (d *Daemon) handle_teardown(w http.ResponseWriter, r *http.Request, purge bool) {
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
	task := func() { done <- d.down(d.base_ctx, e) }
	if purge {
		task = func() { done <- d.purge(d.base_ctx, e) }
	}
	if !e.mbox.post(task) {
		http.Error(w, "container is no longer tracked", http.StatusConflict)
		return
	}
	if err := <-done; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DownResult is the per-devcontainer outcome of a `cld down --all`.
type DownResult struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// handle_down_all stops and removes every devcontainer cld manages, keeping
// volumes and backups. Backs `cld down --all`.
func (d *Daemon) handle_down_all(w http.ResponseWriter, r *http.Request) {
	d.handle_teardown_all(w, r, false)
}

// handle_purge_all stops and removes every devcontainer cld manages and deletes
// each one's named volumes and host-side conversation backup. Backs `cld purge
// --all`.
func (d *Daemon) handle_purge_all(w http.ResponseWriter, r *http.Request) {
	d.handle_teardown_all(w, r, true)
}

// handle_teardown_all backs `cld down --all` and `cld purge --all`. It fans the
// daemon's tracked entries out to their own workers, so removals run
// concurrently and each takes its final backup (down only) before Docker drops
// the container; the per-container outcomes are gathered into the response. It is
// only on the full control plane, never the in-container scoped_api — a managed
// container must not be able to tear the whole fleet down.
//
// The tracked set is only a hint: an entry exists for every started container
// and is declassified as ignored/non-devcontainer only later by ensure (and a
// container that was not running when ensure inspected it is never classified at
// all). So the removal decision is made authoritatively on the worker, against
// the live container: managed_devcontainer re-applies ensure's label/ignore
// gate, and is_tracked drops an entry ensure has since retired. Only entries
// that pass both are removed and reported; anything else is left untouched and
// omitted, so a not-yet-classified or leaked entry for a cld.ignore or plain
// container is never destroyed.
func (d *Daemon) handle_teardown_all(w http.ResponseWriter, _ *http.Request, purge bool) {
	d.mu.Lock()
	entries := make([]*entry, 0, len(d.entries))
	for _, e := range d.entries {
		entries = append(entries, e)
	}
	d.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].snapshot().Name < entries[j].snapshot().Name
	})

	type outcome struct {
		attempted bool
		err       error
	}
	type pending struct {
		id   string
		name string
		done chan outcome
	}
	pends := make([]pending, 0, len(entries))
	for _, e := range entries {
		done := make(chan outcome, 1)
		// Runs on the worker, after any ensure already queued for this entry.
		posted := e.mbox.post(func() {
			if !d.is_tracked(e) || !d.managed_devcontainer(d.base_ctx, e.id) {
				done <- outcome{attempted: false}
				return
			}
			teardown := d.down
			if purge {
				teardown = d.purge
			}
			done <- outcome{attempted: true, err: teardown(d.base_ctx, e)}
		})
		// A worker whose mailbox is already closed (its container was torn down
		// concurrently) is effectively already removed; skip it silently.
		if !posted {
			continue
		}
		pends = append(pends, pending{id: e.id, name: e.snapshot().Name, done: done})
	}

	results := make([]DownResult, 0, len(pends))
	for _, p := range pends {
		oc := <-p.done
		// A container left alone (no longer tracked, or not a cld-managed
		// devcontainer) is not reported as removed.
		if !oc.attempted {
			continue
		}
		res := DownResult{Name: p.name, ID: short(p.id)}
		if oc.err != nil {
			res.Error = oc.err.Error()
		} else {
			res.OK = true
		}
		results = append(results, res)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"results": results})
}

// maxCredentialsLen bounds the accepted credentials body. A ~/.claude
// credentials file is a few hundred bytes; this is generous.
const maxCredentialsLen = 16384

// handle_set_credentials hands the broker the single `/login` it owns, from the
// body of a `~/.claude/.credentials.json` (the claudeAiOauth object). The
// refresh token — the sensitive part — is persisted only here on the daemon
// host, never injected into a container. Sessions authenticate through the
// broker's proxy instead. Backs `cld auth login`.
func (d *Daemon) handle_set_credentials(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCredentialsLen+1))
	if err != nil {
		http.Error(w, "read credentials", http.StatusBadRequest)
		return
	}
	if len(body) > maxCredentialsLen {
		http.Error(w, "credentials too long", http.StatusRequestEntityTooLarge)
		return
	}

	var doc struct {
		ClaudeAiOauth *struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"` // ms since epoch
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.ClaudeAiOauth == nil {
		http.Error(w, "expected a ~/.claude/.credentials.json with a claudeAiOauth object", http.StatusBadRequest)
		return
	}
	if doc.ClaudeAiOauth.RefreshToken == "" {
		http.Error(w, "credentials have no refreshToken", http.StatusBadRequest)
		return
	}

	creds := &broker.Credentials{
		AccessToken:  doc.ClaudeAiOauth.AccessToken,
		RefreshToken: doc.ClaudeAiOauth.RefreshToken,
		ExpiresAt:    time.UnixMilli(doc.ClaudeAiOauth.ExpiresAt),
	}
	if err := d.broker.SetCredentials(creds); err != nil {
		http.Error(w, "store credentials", http.StatusInternalServerError)
		d.log.Warn("set-credentials failed", slog.String("error", err.Error()))
		return
	}
	d.log.Info("broker login updated")
	w.WriteHeader(http.StatusNoContent)
}

// by_name finds a tracked entry by its display name or its short alias. A
// display-name match wins over an alias match, so the handle a user sees under
// NAME always resolves to that same container even if it happens to equal
// another container's alias.
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
	for _, e := range entries {
		if e.snapshot().Alias == name {
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
	items, _, err := fetchItems(ctx, socket, false)
	return items, err
}

// FetchItemsDebug is FetchItems plus the raw captured pane per item ID, the
// sole input to the activity classifier — for diagnosing why a container reads
// as working vs waiting.
func FetchItemsDebug(ctx context.Context, socket string) ([]Item, map[string]string, error) {
	return fetchItems(ctx, socket, true)
}

func fetchItems(ctx context.Context, socket string, debug bool) ([]Item, map[string]string, error) {
	hc := NewSocketClient(socket)
	url := "http://cld/items"
	if debug {
		url += "?debug=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}

	res, err := hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("is `cld serve` running? %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("daemon: %s", res.Status)
	}

	var body struct {
		Items []Item            `json:"items"`
		Panes map[string]string `json:"panes"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, nil, err
	}
	return body.Items, body.Panes, nil
}

// SetActivity reports this session's conversation activity to the daemon over
// the in-container relay socket. Called by `cld x activity <state>` from
// claude's hooks; best-effort by design (the hook wrapper swallows failures).
func SetActivity(ctx context.Context, socket string, state string) error {
	hc := NewSocketClient(socket)
	url := "http://cld/activity?state=" + urlpkg.QueryEscape(state)
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
		return fmt.Errorf("daemon: %s: %s", res.Status, strings.TrimSpace(string(body)))
	}
	return nil
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

// SetCredentials hands the daemon the broker login (the body of a
// ~/.claude/.credentials.json). The credentials travel in the request body (not
// the URL) so they stay out of logs. Backs `cld auth login`; reachable through
// the in-container relay so it works from inside a devcontainer.
func SetCredentials(ctx context.Context, socket string, credentialsJSON string) error {
	hc := NewSocketClient(socket)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://cld/auth/credentials",
		strings.NewReader(credentialsJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

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

// RecreateSession asks the daemon to recreate a devcontainer's session, keeping
// its current proxy-auth mode. Backs `cld it --new`.
func RecreateSession(ctx context.Context, socket string, name string) error {
	return recreateSession(ctx, socket, name, "")
}

// SetProxyMode records whether a project's sessions authenticate through the
// broker proxy (on) or log in per container (off, the default), and recreates
// the session so the change applies at once. Backs `cld up`/`cld it`
// `--proxy`/`--no-proxy`.
func SetProxyMode(ctx context.Context, socket string, name string, on bool) error {
	mode := "off"
	if on {
		mode = "on"
	}
	return recreateSession(ctx, socket, name, mode)
}

func recreateSession(ctx context.Context, socket string, name string, proxy string) error {
	hc := NewSocketClient(socket)
	url := "http://cld/session/new?name=" + urlpkg.QueryEscape(name)
	if proxy != "" {
		url += "&proxy=" + proxy
	}
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

// DownAll asks the daemon to stop and remove every devcontainer it manages,
// returning the per-container outcome. Containers cld does not manage — those
// without the devcontainer label, or excluded by the cld.ignore label or an
// ignore glob — are never tracked by the daemon, so they are never touched. It
// allows a generous timeout because each removal takes a final backup and a
// Compose teardown can be slow, and several run at once.
func DownAll(ctx context.Context, socket string) ([]DownResult, error) {
	hc := NewSocketClient(socket)
	hc.Timeout = 10 * time.Minute
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://cld/down/all", nil)
	if err != nil {
		return nil, err
	}

	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("is `cld serve` running? %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("daemon: %s: %s", res.Status, string(body))
	}

	var body struct {
		Results []DownResult `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Results, nil
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

// Purge asks the daemon to stop and remove a devcontainer and to delete its
// named volumes and host-side conversation backup — the irreversible superset of
// Down. It uses the same generous timeout as Down because tearing down a Compose
// project and removing volumes can take a while.
func Purge(ctx context.Context, socket string, name string) error {
	hc := NewSocketClient(socket)
	hc.Timeout = 2 * time.Minute
	url := "http://cld/purge?name=" + urlpkg.QueryEscape(name)
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

// PurgeAll asks the daemon to stop and remove every devcontainer it manages and
// to delete each one's named volumes and host-side conversation backup,
// returning the per-container outcome. Like DownAll it never touches containers
// cld does not manage, and allows a generous timeout because several teardowns —
// each including volume removal — run at once.
func PurgeAll(ctx context.Context, socket string) ([]DownResult, error) {
	hc := NewSocketClient(socket)
	hc.Timeout = 10 * time.Minute
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://cld/purge/all", nil)
	if err != nil {
		return nil, err
	}

	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("is `cld serve` running? %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("daemon: %s: %s", res.Status, string(body))
	}

	var body struct {
		Results []DownResult `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Results, nil
}
