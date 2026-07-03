package config

import (
	"encoding"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Duration time.Duration

var (
	_ encoding.TextUnmarshaler = (*Duration)(nil)
	_ encoding.TextMarshaler   = (*Duration)(nil)
)

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}

	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

type AuthConfig struct {
	// Path to a host file containing a Claude Code OAuth token (as produced by
	// `claude setup-token`). When set, the token is injected as
	// CLAUDE_CODE_OAUTH_TOKEN into each session so a fresh container needs no
	// interactive login. The path (not the token) is all that appears in the
	// tmux command; keep the file mode 0600.
	OAuthTokenFile string `yaml:"oauth_token_file"`
}

type UpConfig struct {
	// Image used to run the devcontainer CLI when neither `devcontainer` nor
	// `npx` is available on the host.
	Image string `yaml:"image"`
}

type ReleaseConfig struct {
	// Base URL of the Claude Code release channel.
	BaseURL string `yaml:"base_url"`
	// Release channel to follow: "stable" or "latest".
	Channel string `yaml:"channel"`
	// How often to check the channel for a new version.
	CheckInterval Duration `yaml:"check_interval"`
}

type SyncConfig struct {
	// Delay after a change before the conversation state is copied out,
	// coalescing bursts of file events into a single copy.
	Debounce Duration `yaml:"debounce"`
	// Polling interval used when the in-container watcher cannot run
	// (container architecture differs from the host).
	FallbackInterval Duration `yaml:"fallback_interval"`
}

func (c *Config) evaluateCld() error {
	if c.CacheDir == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		c.CacheDir = filepath.Join(d, "cld")
	}
	if c.DataDir == "" {
		d := os.Getenv("XDG_DATA_HOME")
		if d == "" {
			h, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			d = filepath.Join(h, ".local", "share")
		}
		c.DataDir = filepath.Join(d, "cld")
	}

	// Expand a leading "~" so the documented defaults work when copied
	// verbatim from cld.yaml into a config file.
	for _, p := range []*string{&c.CacheDir, &c.DataDir, &c.Auth.OAuthTokenFile} {
		v, err := expandTilde(*p)
		if err != nil {
			return err
		}
		*p = v
	}

	if c.Release.BaseURL == "" {
		c.Release.BaseURL = "https://downloads.claude.ai/claude-code-releases"
	}
	if c.Release.Channel == "" {
		c.Release.Channel = "stable"
	}
	if c.Release.CheckInterval == 0 {
		c.Release.CheckInterval = Duration(time.Hour)
	}

	if c.Sync.Debounce == 0 {
		c.Sync.Debounce = Duration(3 * time.Second)
	}
	if c.Sync.FallbackInterval == 0 {
		c.Sync.FallbackInterval = Duration(time.Minute)
	}

	if c.Up.Image == "" {
		c.Up.Image = "ghcr.io/lesomnus/cld:runner"
	}

	return nil
}

// expandTilde replaces a leading "~/" (or a bare "~") with the home directory.
func expandTilde(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if p == "~" {
		return h, nil
	}
	return filepath.Join(h, p[2:]), nil
}

// SocketPath is the path to the unix socket the daemon serves its HTTP API on.
func (c *Config) SocketPath() string {
	return filepath.Join(c.CacheDir, "cld.sock")
}

// TmuxSocketPath is the path to the socket of the dedicated tmux server.
func (c *Config) TmuxSocketPath() string {
	return filepath.Join(c.CacheDir, "tmux.sock")
}

// BinDir is the root of the claude binary cache,
// laid out as <BinDir>/<version>/<platform>/claude.
func (c *Config) BinDir() string {
	return filepath.Join(c.CacheDir, "bin")
}

// GlobalBackupDir holds project-independent state such as credentials and settings.
func (c *Config) GlobalBackupDir() string {
	return filepath.Join(c.DataDir, "global")
}

// ProjectBackupDir holds per-project state such as conversation transcripts,
// keyed by a digest of the host-side workspace path.
func (c *Config) ProjectBackupDir(key string) string {
	return filepath.Join(c.DataDir, "projects", key)
}
