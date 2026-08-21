package daemon

import (
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/lesomnus/cld/cmd/config"
)

// The session policy — what a container is provisioned with: env, files,
// scripts, projects, the engine settings, and the ignore/gh/dotfiles switches —
// is re-read from the config file while the daemon runs. Editing cld.yaml
// therefore takes effect on the next provisioning, with no restart.
//
// The rest of the config keeps the values it was started with: the directories,
// auth, telemetry and sync settings bind sockets, relays and watchers that live
// as long as the daemon, and swapping those underneath is a different feature
// with different risks.
//
// This also settles an inconsistency that was genuinely confusing: the engine
// override (cld.dind.yaml) has always been read from disk on every reconcile,
// so one file in the config directory applied immediately while the other
// needed a restart, with nothing to say which was which.
type policy_store struct {
	mu sync.Mutex
	// paths to watch, in order; the first that exists is the config. Empty
	// means "do not reload" — what a daemon built in tests wants.
	paths []string
	path  string // what was loaded last, "" when none existed
	stamp string // mtime+size of that file, to notice an edit
	cur   atomic.Pointer[config.Config]
	log   *slog.Logger
}

func new_policy_store(cfg *config.Config, log *slog.Logger) *policy_store {
	s := &policy_store{log: log}
	s.cur.Store(cfg)
	if p := cfg.Path(); p != "" {
		s.paths = []string{p}
		s.path, s.stamp = p, file_stamp(p)
	}
	return s
}

// watch sets the paths to look at, for a daemon whose config file did not exist
// at startup: creating it later is then picked up like any other edit.
func (s *policy_store) watch(paths []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.paths) == 0 {
		s.paths = paths
	}
}

func (s *policy_store) get() *config.Config { return s.cur.Load() }

// refresh re-reads the config when the file changed since the last look. It is
// called once per provisioning pass, so it costs one stat.
//
// A file that stops parsing keeps the last good policy — a typo must not wipe
// the settings out from under every container — and is reported once, since the
// stamp only moves when the file is edited again.
func (s *policy_store) refresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.paths) == 0 {
		return
	}

	path, stamp := "", ""
	for _, p := range s.paths {
		if st := file_stamp(p); st != "" {
			path, stamp = p, st
			break
		}
	}
	if path == s.path && stamp == s.stamp {
		return
	}
	s.path, s.stamp = path, stamp

	if path == "" {
		// The file was removed: mirror that rather than keeping settings the
		// user believes they deleted.
		c := &config.Config{}
		if err := c.Evaluate(); err != nil {
			s.log.Warn("config: cannot fall back to defaults", slog.String("error", err.Error()))
			return
		}
		s.cur.Store(c)
		s.log.Info("config removed; using defaults")
		return
	}

	c, err := config.ReadFromFile(path)
	if err == nil {
		err = c.Evaluate()
	}
	if err != nil {
		s.log.Warn("config: keeping the previous one",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	s.cur.Store(c)
	s.log.Info("config reloaded", slog.String("path", path))
}

// file_stamp identifies a file's content well enough to notice an edit, and is
// "" when it does not exist.
func file_stamp(p string) string {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return ""
	}
	return fi.ModTime().UTC().Format("20060102150405.000000000") + ":" + strconv.FormatInt(fi.Size(), 10)
}
