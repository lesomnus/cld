package daemon

import (
	"context"
	"fmt"
	"log/slog"
)

// update_claude re-injects the current Claude Code binary into the container
// and recreates the session so the new binary takes effect. It runs on the
// container's worker goroutine and returns the version now installed. channel
// selects the release channel: "" is the daemon's tracked channel (the caller
// refreshes it first, see refresh_release), a non-empty channel (e.g. "latest")
// is resolved fresh for this one install without changing what the daemon tracks.
//
// install_claude points the "claude" symlink at claude-<version> atomically, so
// it never overwrites a running binary and a live session keeps the version it
// started with; the recreate_session is what actually swaps a running session
// onto the freshly installed one. Backs `cld update`.
func (d *Daemon) update_claude(ctx context.Context, e *entry, channel string) (string, error) {
	if e.cfg_dir == "" || e.platform == "" || e.item.Workspace == "" {
		return "", fmt.Errorf("container %q is not provisioned yet", e.item.Name)
	}

	version, err := d.install_claude(ctx, e, e.id, channel)
	if err != nil {
		return "", fmt.Errorf("install claude: %w", err)
	}
	e.version = version
	e.item.Version = version

	// Replace the running session with one launched from the freshly installed
	// binary. This kills the old claude process and starts a new one; a user
	// attached to the session is detached and reattaches to the new one.
	if err := d.recreate_session(ctx, e); err != nil {
		return version, fmt.Errorf("recreate session: %w", err)
	}

	d.log.Info("claude updated",
		slog.String("id", short(e.id)),
		slog.String("name", e.item.Name),
		slog.String("version", version))
	return version, nil
}

// refresh_release forces a fresh channel re-resolve so an update installs the
// newest version the channel offers, not the version the daemon cached on its
// last hourly check. Best-effort: on failure the subsequent install falls back
// to the cached version (see release.Manager.Current), so an update still
// re-injects and restarts with whatever is already known.
func (d *Daemon) refresh_release(ctx context.Context) {
	if err := d.rel.Refresh(ctx); err != nil {
		d.log.Warn("release refresh failed; using cached version",
			slog.String("error", err.Error()))
	}
}

// updateOutcome is the result of one worker's update task, gathered by the HTTP
// handlers. skip marks an entry that was not provisioned (stopped or not yet
// ready) and so has no session to update; it is omitted from the response.
type updateOutcome struct {
	version string
	err     error
	skip    bool
}
