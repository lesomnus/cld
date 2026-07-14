package daemon

import (
	"os"
	"path/filepath"
)

// proxyStore records, per project (keyed by backup_key), whether that
// project's sessions authenticate through the broker's auth proxy instead of
// the default per-container login. It is a durable user preference — set by
// `cld up --proxy` / `cld it --proxy` and cleared by `--no-proxy` — so it lives
// under DataDir and survives daemon and container restarts. The presence of the
// key's marker file means proxy-on; its absence means the default (off).
//
// The proxy is opt-in because it points the session's ANTHROPIC_BASE_URL at a
// non-first-party endpoint, which makes Claude Code degrade its UI and disable
// some features. Most projects are better served by the default per-container
// login (whose credentials cld persists per project); the proxy exists for
// sharing one subscription across sessions when that trade-off is worth it.
type proxyStore struct {
	dir string
}

func (s *proxyStore) path(key string) string {
	return filepath.Join(s.dir, key)
}

// get reports whether proxy mode is enabled for the project key.
func (s *proxyStore) get(key string) bool {
	if key == "" {
		return false
	}
	_, err := os.Stat(s.path(key))
	return err == nil
}

// set turns proxy mode on or off for the project key, persisting the choice.
func (s *proxyStore) set(key string, on bool) error {
	if key == "" {
		return nil
	}
	if !on {
		if err := os.Remove(s.path(key)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path(key), nil, 0o644)
}

// clear drops any proxy preference for the project key; used by purge so a
// removed project leaves nothing behind.
func (s *proxyStore) clear(key string) {
	os.Remove(s.path(key))
}
