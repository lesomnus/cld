package release

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Manager tracks the channel's current version and serves binaries from the
// cache. When the channel is unreachable it falls back to whatever version
// is already cached so provisioning keeps working offline.
type Manager struct {
	Client  *Client
	Cache   *Cache
	Channel string
	Log     *slog.Logger

	mu      sync.Mutex
	version string
	gc_done string // last version the cache was GC'd down to
}

// Refresh re-resolves the channel version. On failure the previous
// (or cached) version stays in effect.
func (m *Manager) Refresh(ctx context.Context) error {
	v, err := m.Client.Version(ctx, m.Channel)
	if err != nil {
		return err
	}

	m.mu.Lock()
	changed := m.version != v
	m.version = v
	m.mu.Unlock()

	if changed && m.Log != nil {
		m.Log.Info("release version resolved", slog.String("version", v))
	}
	return nil
}

// RefreshLoop calls Refresh periodically until the context is done.
func (m *Manager) RefreshLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Refresh(ctx); err != nil && m.Log != nil {
				m.Log.Warn("release version check failed", slog.String("error", err.Error()))
			}
		}
	}
}

// Current returns the version to provision. If the channel has never been
// resolved it tries once, then falls back to a cached version.
func (m *Manager) Current(ctx context.Context) (string, error) {
	m.mu.Lock()
	v := m.version
	m.mu.Unlock()
	if v != "" {
		return v, nil
	}

	if err := m.Refresh(ctx); err != nil {
		vs := m.Cache.Versions()
		if len(vs) == 0 {
			return "", fmt.Errorf("resolve version: %w (and no cached version to fall back to)", err)
		}
		v = pick_newest(vs)
		if m.Log != nil {
			m.Log.Warn("channel unreachable; using cached version",
				slog.String("version", v), slog.String("error", err.Error()))
		}

		m.mu.Lock()
		m.version = v
		m.mu.Unlock()
		return v, nil
	}

	m.mu.Lock()
	v = m.version
	m.mu.Unlock()
	return v, nil
}

// Ensure returns the path to the current version's binary for the platform,
// downloading it if needed, and garbage-collects older cached versions.
func (m *Manager) Ensure(ctx context.Context, p Platform) (string, string, error) {
	v, err := m.Current(ctx)
	if err != nil {
		return "", "", err
	}

	path, err := m.Cache.Ensure(ctx, v, p)
	if err != nil {
		return "", "", err
	}

	// GC only when the current version changed since the last sweep, and only
	// after the new version is fully cached. This avoids re-scanning the cache
	// on every provision and avoids deleting a version a concurrent provision
	// of a different platform may still be downloading. A provision racing an
	// actual version bump re-fetches via install's ENOENT retry.
	m.mu.Lock()
	need_gc := m.gc_done != v
	m.mu.Unlock()
	if need_gc {
		if err := m.Cache.GC(v); err != nil {
			if m.Log != nil {
				m.Log.Warn("cache GC failed", slog.String("error", err.Error()))
			}
		} else {
			m.mu.Lock()
			m.gc_done = v
			m.mu.Unlock()
		}
	}
	return v, path, nil
}

// pick_newest picks the largest version by naive semver-ish comparison.
func pick_newest(vs []string) string {
	best := vs[0]
	for _, v := range vs[1:] {
		if compare_versions(v, best) > 0 {
			best = v
		}
	}
	return best
}

func compare_versions(a, b string) int {
	pa, pb := parse_version(a), parse_version(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parse_version(v string) [3]int {
	var out [3]int
	i, n := 0, 0
	for _, c := range v {
		switch {
		case c >= '0' && c <= '9':
			n = n*10 + int(c-'0')
		case c == '.' && i < 2:
			out[i] = n
			n = 0
			i++
		default:
			out[i] = n
			return out
		}
	}
	out[i] = n
	return out
}
