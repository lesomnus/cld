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
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/cld/internal/broker"
	"github.com/lesomnus/cld/internal/dockerx"
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
	mux.HandleFunc("POST /claude/update", d.handle_update)
	mux.HandleFunc("POST /claude/update/all", d.handle_update_all)
	mux.HandleFunc("GET /claude/config", d.handle_get_config)
	mux.HandleFunc("GET /session/env", d.handle_get_env)
	// Host-only: the config is global, so a container reading it would see
	// every other project's settings — and its secrets.
	mux.HandleFunc("GET /config", d.handle_get_config_file)
	// Host-only: driving the engine means exec'ing into its container, which
	// needs the host engine — something a container has no access to anyway.
	mux.HandleFunc("GET /docker/engine", d.handle_get_engine)
	mux.HandleFunc("POST /auth/credentials", d.handle_set_credentials)
	mux.HandleFunc("GET /usage", d.handle_usage(""))
	return mux
}

// handle_usage reports subscription usage for every login the daemon can see
// (the broker login and each ready session's own login); see Daemon.Usage. The
// per-source results are memoized, so polling this is cheap. selfID scopes the
// report: "" on the trusted host API (all logins), or a container id on the
// in-container relay (only that container's own login).
func (d *Daemon) handle_usage(selfID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		report := d.Usage(r.Context(), selfID)
		// The host report carries the fleet-wide weekly token tally: adopt the
		// subscription's weekly reset as this window's boundary (a no-op once
		// adopted, and the trigger that rolls the totals over when it passes),
		// then attach the current totals. The per-container relay report (selfID
		// set, a single scoped login) leaves Weekly zero.
		if selfID == "" {
			d.week.anchor(weeklyReset(report))
			report.Weekly = d.week.get()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(report)
	}
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
	// A container may update its own Claude Code binary and restart its own
	// session; the whole-fleet /claude/update/all is deliberately host-only.
	mux.HandleFunc("POST /claude/update", only_self(d.handle_update))
	// A container may read its own effective config, and the environment its
	// own sessions run with — which its processes already hold anyway.
	mux.HandleFunc("GET /claude/config", only_self(d.handle_get_config))
	mux.HandleFunc("GET /session/env", only_self(d.handle_get_env))
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
	// Usage is self-scoped: a container sees only its own login's usage (and the
	// broker login's, but only if it is itself a broker session), never another
	// project's. The scope is the bound self_id, not a caller argument.
	mux.HandleFunc("GET /usage", d.handle_usage(self_id))
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

	// Nothing is left to share the engine, so it goes too — a privileged
	// container should not outlive the last devcontainer it served. A purge
	// takes its accumulated cache with it; a down leaves that for next time.
	d.remove_shared_dind(d.base_ctx, purge)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"results": results})
}

// UpdateResult is the per-devcontainer outcome of a `cld update` / `--all`.
type UpdateResult struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// handle_update re-injects the current Claude Code binary into one
// devcontainer, by display name, and recreates its session so the new binary
// takes effect. The release channel is re-resolved first so the newest version
// is installed, not the daemon's hourly-cached one. Backs `cld update`.
func (d *Daemon) handle_update(w http.ResponseWriter, r *http.Request) {
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

	// An explicit ?channel= is resolved fresh by install_claude, so only the
	// tracked-channel path needs a forced refresh (best-effort: on failure the
	// install falls back to the cached version).
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		d.refresh_release(r.Context())
	}

	done := make(chan updateOutcome, 1)
	if !e.mbox.post(func() {
		v, err := d.update_claude(d.base_ctx, e, channel)
		done <- updateOutcome{version: v, err: err}
	}) {
		http.Error(w, "container is no longer tracked", http.StatusConflict)
		return
	}
	oc := <-done
	if oc.err != nil {
		http.Error(w, oc.err.Error(), http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(UpdateResult{
		Name: e.snapshot().Name, ID: short(e.id), OK: true, Version: oc.version,
	})
}

// handle_update_all re-injects the current Claude Code binary into every
// devcontainer cld manages and recreates each session. It re-resolves the
// channel once, then fans the tracked entries out to their own workers so the
// installs and restarts run concurrently. A container that is not provisioned
// (stopped, or not yet ready) has no session to update and is skipped, so it is
// omitted from the response. It is only on the full control plane, never the
// in-container scoped_api — a managed container must not restart the whole
// fleet's sessions.
func (d *Daemon) handle_update_all(w http.ResponseWriter, r *http.Request) {
	// An explicit ?channel= is resolved fresh per install; otherwise refresh the
	// tracked channel once for the whole fleet rather than per worker.
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		d.refresh_release(r.Context())
	}

	d.mu.Lock()
	entries := make([]*entry, 0, len(d.entries))
	for _, e := range d.entries {
		entries = append(entries, e)
	}
	d.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].snapshot().Name < entries[j].snapshot().Name
	})

	type pending struct {
		id   string
		name string
		done chan updateOutcome
	}
	pends := make([]pending, 0, len(entries))
	for _, e := range entries {
		done := make(chan updateOutcome, 1)
		// Runs on the worker, after any ensure already queued for this entry, so
		// the provisioned check reads settled state.
		posted := e.mbox.post(func() {
			if e.cfg_dir == "" || e.platform == "" || e.item.Workspace == "" {
				done <- updateOutcome{skip: true}
				return
			}
			v, err := d.update_claude(d.base_ctx, e, channel)
			done <- updateOutcome{version: v, err: err}
		})
		// A worker whose mailbox is already closed (its container was torn down
		// concurrently) is effectively gone; skip it silently.
		if !posted {
			continue
		}
		pends = append(pends, pending{id: e.id, name: e.snapshot().Name, done: done})
	}

	results := make([]UpdateResult, 0, len(pends))
	for _, p := range pends {
		oc := <-p.done
		// A container with no session to update (not provisioned) is not reported.
		if oc.skip {
			continue
		}
		res := UpdateResult{Name: p.name, ID: short(p.id), Version: oc.version}
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

// configFiles is the allowlist of config-dir files `cld cat` may read: the
// same user-managed files `cld edit` writes. It is an allowlist, not an
// arbitrary path, so the endpoint can never be turned into a read-any-file probe
// of a container. The values are the on-disk names under the config dir.
var configFiles = map[string]bool{"settings.json": true, "CLAUDE.md": true}

// handle_get_config returns the raw bytes of one config-dir file as it actually
// exists inside a devcontainer — the effective config claude uses there, i.e.
// the user-default base after cld sanitized it and merged its own keys. Backs
// `cld cat`. The read runs on the container's worker so it sees settled
// cfg_dir/id state and never races provisioning.
func (d *Daemon) handle_get_config(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	file := r.URL.Query().Get("file")
	if file == "" {
		file = "settings.json"
	}
	if !configFiles[file] {
		http.Error(w, "unsupported file", http.StatusBadRequest)
		return
	}

	e := d.by_name(name)
	if e == nil {
		http.Error(w, "no such devcontainer", http.StatusNotFound)
		return
	}

	type result struct {
		data        []byte
		found       bool
		provisioned bool
		err         error
	}
	done := make(chan result, 1)
	if !e.mbox.post(func() {
		if e.cfg_dir == "" {
			done <- result{}
			return
		}
		data, ok, err := dockerx.ReadFile(d.base_ctx, d.cli, e.id, path.Join(e.cfg_dir, file))
		done <- result{data: data, found: ok, provisioned: true, err: err}
	}) {
		http.Error(w, "container is no longer tracked", http.StatusConflict)
		return
	}

	res := <-done
	if res.err != nil {
		http.Error(w, res.err.Error(), http.StatusInternalServerError)
		return
	}
	if !res.provisioned {
		http.Error(w, "devcontainer is not provisioned yet", http.StatusConflict)
		return
	}
	if !res.found {
		http.Error(w, fmt.Sprintf("%s: no such file in this devcontainer", file), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(res.data)
}

// EnvVar is one variable of a session's effective environment, with the layer
// that decided it (see daemon.session_env). Backs `cld setting env`.
type EnvVar struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Origin string `json:"origin"`
	// Unset means the config removed a variable the container had, so the
	// session runs without it.
	Unset bool `json:"unset,omitempty"`
}

// handle_get_env returns the environment a session of this devcontainer runs
// with, resolved exactly as ensure_session resolves it, each variable carrying
// the layer that decided it. Without this a user has no way to tell why a
// variable they set in cld.yaml did not take. Like handle_get_config it runs on
// the container's worker, so it never races provisioning.
func (d *Daemon) handle_get_env(w http.ResponseWriter, r *http.Request) {
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

	type result struct {
		vars        []EnvVar
		provisioned bool
	}
	done := make(chan result, 1)
	if !e.mbox.post(func() {
		if e.cfg_dir == "" {
			done <- result{}
			return
		}
		res := d.session_env(e)
		vars := make([]EnvVar, 0, len(res.Vars))
		for _, v := range res.Vars {
			vars = append(vars, EnvVar{Key: v.Key, Value: v.Value, Origin: v.Origin, Unset: v.Unset})
		}
		done <- result{vars: vars, provisioned: true}
	}) {
		http.Error(w, "container is no longer tracked", http.StatusConflict)
		return
	}

	res := <-done
	if !res.provisioned {
		http.Error(w, "devcontainer is not provisioned yet", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"vars": res.vars})
}

// DaemonConfig is the configuration the daemon is actually running with, and
// the file it came from ("" when none was found). Backs `cld config --daemon`,
// which exists because the alternative is guessing: the daemon reads its config
// on the host, from a path a client may not share, so "the setting did not
// apply" has no other answer.
type DaemonConfig struct {
	Path string `json:"path"`
	YAML string `json:"yaml"`
}

func (d *Daemon) handle_get_config_file(w http.ResponseWriter, r *http.Request) {
	b, err := yaml.Marshal(d.cfg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(DaemonConfig{Path: d.cfg.Path(), YAML: string(b)})
}

// DockerEngine describes the Docker engine cld runs for a devcontainer.
type DockerEngine struct {
	// Container is the engine container's id, which is what a client execs
	// into to drive it — the engine is reachable only from the devcontainer's
	// private network, never from the host.
	Container string `json:"container"`
	// Name is the engine container's name, and Endpoint what the session's
	// DOCKER_HOST points at.
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Running  bool   `json:"running"`
}

// handle_get_engine reports the engine cld runs for a devcontainer, so
// `cld docker` can drive it. It is a lookup, not a start: an engine appears
// when the container is provisioned with `docker: {mode: dind}`.
func (d *Daemon) handle_get_engine(w http.ResponseWriter, r *http.Request) {
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

	type result struct {
		engine     DockerEngine
		configured bool
		err        error
	}
	done := make(chan result, 1)
	// On the container's worker, like the other read endpoints: the project key
	// is derived from worker-owned fields, so reading them here would race
	// provisioning.
	if !e.mbox.post(func() {
		if !d.cfg.DockerFor(e.item.LocalFolder).Enabled() {
			done <- result{}
			return
		}
		key := d.backup_key(e)
		id, err := d.find_dind(d.base_ctx, key)
		done <- result{
			configured: true,
			err:        err,
			engine: DockerEngine{
				Container: id,
				Name:      dind_container_name(key),
				Endpoint:  dind_endpoint(key),
				Running:   id != "" && d.dind_running(d.base_ctx, id),
			},
		}
	}) {
		http.Error(w, "container is no longer tracked", http.StatusConflict)
		return
	}

	res := <-done
	if res.err != nil {
		http.Error(w, res.err.Error(), http.StatusInternalServerError)
		return
	}
	if !res.configured {
		http.Error(w, "no engine: this project is configured with `docker: {mode: off}`",
			http.StatusConflict)
		return
	}
	if res.engine.Container == "" {
		http.Error(w, "no engine yet: the devcontainer has not been provisioned with one",
			http.StatusConflict)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res.engine)
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

// by_name finds a tracked entry by its managed name, its short alias, or the
// display label shown under NAME. The managed name wins over an alias, which
// wins over a display label, so the unique handles always resolve to their own
// container before the non-unique display label is consulted — and the label a
// user sees under NAME still resolves even when it differs from both (a
// namespaced project shows "cld" but is named "lesomnus-cld").
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
	for _, e := range entries {
		if e.snapshot().Display == name {
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

// FetchUsage asks the daemon for subscription usage across every login it can
// see (the broker login and each ready session's login). Backs `cld usage` and
// the usage line in `cld watch`.
func FetchUsage(ctx context.Context, socket string) (*UsageReport, error) {
	hc := NewSocketClient(socket)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cld/usage", nil)
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

	var report UsageReport
	if err := json.NewDecoder(res.Body).Decode(&report); err != nil {
		return nil, err
	}
	return &report, nil
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

// UpdateClaude asks the daemon to re-inject the current Claude Code binary into
// one devcontainer and recreate its session so the new binary takes effect. The
// daemon re-resolves the release channel first, so this installs the newest
// version rather than the daemon's hourly-cached one. A non-empty channel
// (e.g. "latest") overrides the daemon's configured channel for this install
// only, without changing what the daemon tracks. Returns the outcome (including
// the version now installed). Backs `cld update`. It allows a generous timeout
// because a version bump downloads the binary before it is injected.
func UpdateClaude(ctx context.Context, socket string, name string, channel string) (UpdateResult, error) {
	hc := NewSocketClient(socket)
	hc.Timeout = 5 * time.Minute
	url := "http://cld/claude/update?name=" + urlpkg.QueryEscape(name)
	if channel != "" {
		url += "&channel=" + urlpkg.QueryEscape(channel)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return UpdateResult{}, err
	}

	res, err := hc.Do(req)
	if err != nil {
		return UpdateResult{}, fmt.Errorf("is `cld serve` running? %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return UpdateResult{}, fmt.Errorf("daemon: %s: %s", res.Status, string(body))
	}

	var out UpdateResult
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return UpdateResult{}, err
	}
	return out, nil
}

// GetClaudeConfig returns the raw bytes of a config-dir file (file "" means
// settings.json) as it actually exists inside the named devcontainer — the
// effective config claude uses there. A missing file or unknown devcontainer
// surfaces as the daemon's own message. Backs `cld cat`.
func GetClaudeConfig(ctx context.Context, socket string, name string, file string) ([]byte, error) {
	hc := NewSocketClient(socket)
	url := "http://cld/claude/config?name=" + urlpkg.QueryEscape(name)
	if file != "" {
		url += "&file=" + urlpkg.QueryEscape(file)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}
	return io.ReadAll(res.Body)
}

// GetSessionEnv returns the environment a session of the named devcontainer
// runs with, each variable carrying the layer that decided it. Backs
// `cld setting env`.
func GetSessionEnv(ctx context.Context, socket string, name string) ([]EnvVar, error) {
	hc := NewSocketClient(socket)
	url := "http://cld/session/env?name=" + urlpkg.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}

	var out struct {
		Vars []EnvVar `json:"vars"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Vars, nil
}

// GetDaemonConfig returns the configuration the daemon loaded, and from where.
// Backs `cld config --daemon`.
func GetDaemonConfig(ctx context.Context, socket string) (DaemonConfig, error) {
	hc := NewSocketClient(socket)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://cld/config", nil)
	if err != nil {
		return DaemonConfig{}, err
	}

	res, err := hc.Do(req)
	if err != nil {
		return DaemonConfig{}, fmt.Errorf("is `cld serve` running? %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		msg := strings.TrimSpace(string(body))
		if res.StatusCode == http.StatusNotFound {
			return DaemonConfig{}, fmt.Errorf(
				"%s — the running daemon may predate `cld config --daemon`; re-run `cld install --recreate`", msg)
		}
		return DaemonConfig{}, fmt.Errorf("%s", msg)
	}

	var out DaemonConfig
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return DaemonConfig{}, err
	}
	return out, nil
}

// GetDockerEngine returns the Docker engine cld runs for the named
// devcontainer. Backs `cld docker`.
func GetDockerEngine(ctx context.Context, socket string, name string) (DockerEngine, error) {
	hc := NewSocketClient(socket)
	url := "http://cld/docker/engine?name=" + urlpkg.QueryEscape(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return DockerEngine{}, err
	}

	res, err := hc.Do(req)
	if err != nil {
		return DockerEngine{}, fmt.Errorf("is `cld serve` running? %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		body := strings.TrimSpace(string(raw))
		// A daemon older than this CLI has no such route, and its router's bare
		// "404 page not found" explains nothing on its own.
		if res.StatusCode == http.StatusNotFound && !strings.Contains(body, "devcontainer") {
			return DockerEngine{}, fmt.Errorf(
				"%s — the running daemon may predate `cld docker`; re-run `cld install --recreate`", body)
		}
		return DockerEngine{}, fmt.Errorf("%s", body)
	}

	var out DockerEngine
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return DockerEngine{}, err
	}
	return out, nil
}

// UpdateClaudeAll asks the daemon to re-inject the current Claude Code binary
// into every devcontainer it manages and recreate each session, returning the
// per-container outcome. Containers cld does not manage are never tracked by the
// daemon, so they are never touched; a container without a live session (stopped
// or not yet ready) is skipped and omitted from the results. Backs
// `cld update --all`. A non-empty channel (e.g. "latest") overrides the daemon's
// configured channel for these installs only. It allows a generous timeout
// because each may download the binary and several run at once.
func UpdateClaudeAll(ctx context.Context, socket string, channel string) ([]UpdateResult, error) {
	hc := NewSocketClient(socket)
	hc.Timeout = 10 * time.Minute
	url := "http://cld/claude/update/all"
	if channel != "" {
		url += "?channel=" + urlpkg.QueryEscape(channel)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
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

	var out struct {
		Results []UpdateResult `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Results, nil
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
