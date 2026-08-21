package config

import (
	"os"
	"path/filepath"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/z"
)

// DefaultConfigPaths are the config files looked for, in order, when no
// --config is given: the user's config directory, and nothing else.
//
// The working directory is deliberately NOT searched. A cld.yaml sitting in
// whatever checkout you happen to be in would be loaded by the client while the
// daemon — which runs in a container and cannot see it — kept using its own, so
// the two would disagree with no sign of it, and `cld config edit` would write
// somewhere nothing reads. Point at such a file with --config when you mean to.
func DefaultConfigPaths() []string {
	dir := UserConfigDir()
	if dir == "" {
		return nil
	}
	return []string{filepath.Join(dir, "cld.yaml"), filepath.Join(dir, "cld.yml")}
}

// UserConfigDirName is where a user's cld config lives under the home
// directory. It is spelled out rather than resolved through $XDG_CONFIG_HOME so
// the host CLI and the daemon always agree on one path: the daemon sees the
// host home through a read-only mount and has no way to know what
// XDG_CONFIG_HOME meant on the host.
const UserConfigDirName = ".config/cld"

// UserConfigDir is the user's cld config directory as the running process can
// reach it: under the mounted host home when this is the containerized daemon,
// and under this user's own home otherwise. Returns "" when neither is known.
//
// This is what lets `cld install` keep mounting nothing but the cache, data and
// home directories while the daemon still reads the config the user edits on
// the host.
func UserConfigDir() string {
	if h := os.Getenv(HostHomeEnv); h != "" {
		return filepath.Join(h, UserConfigDirName)
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, UserConfigDirName)
}

const (
	// HostHomeMount is the container path where `cld install` mounts the host
	// user's home directory (only the home, not the whole host root), read-only,
	// so the daemon — which runs inside a container — can still read host-side
	// files such as ~/.dotfiles.
	HostHomeMount = "/host-home"
	// HostHomeEnv carries HostHomeMount into the daemon; its presence is also
	// what tells `cld serve` it is running as the intended container.
	HostHomeEnv = "CLD_HOST_HOME"
)

type Config struct {
	path string

	// Directory for disposable data: binary cache and sockets.
	// Defaults to "$XDG_CACHE_HOME/cld".
	CacheDir string `yaml:"cache_dir"`
	// Directory for data that must not be lost: conversation backups.
	// Defaults to "$XDG_DATA_HOME/cld".
	DataDir string `yaml:"data_dir"`

	// Glob patterns matched against the host-side workspace path
	// (the "devcontainer.local_folder" label) to exclude from provisioning.
	Ignore []string `yaml:"ignore"`

	// HostHome is the container path where the host user's home directory is
	// mounted read-only (see HostHomeMount), sourced from CLD_HOST_HOME. It lets
	// the daemon read host-side files such as ~/.dotfiles despite running inside
	// a container. Empty when the daemon runs without that mount. Not a user
	// knob — it is wired in by `cld install` / docker-compose.
	HostHome string `yaml:"-"`

	// Env, Files, Scripts and Projects declare what a claude session runs
	// with: its environment, files placed in the container for it, and
	// scripts run before it starts. Env/Files/Scripts apply to every managed
	// container; Projects scopes the same three to matching workspaces. See
	// session.go and docs/session-env.md.
	Env      EnvMap          `yaml:"env"`
	Files    []FileSpec      `yaml:"files"`
	Scripts  ScriptSet       `yaml:"scripts"`
	Projects []ProjectConfig `yaml:"projects"`

	// Docker declares whether a session gets a Docker engine of its own. Off
	// by default; see docker.go and docs/session-docker.md.
	Docker DockerConfig `yaml:"docker"`

	Auth      AuthConfig      `yaml:"auth"`
	Release   ReleaseConfig   `yaml:"release"`
	Gh        GhConfig        `yaml:"gh"`
	Dotfiles  DotfilesConfig  `yaml:"dotfiles"`
	Telemetry TelemetryConfig `yaml:"telemetry"`
	Sync      SyncConfig      `yaml:"sync"`
	Up        UpConfig        `yaml:"up"`
	Install   InstallConfig   `yaml:"install"`

	Otel OtelConfig
}

func ReadFromFile(p string) (*Config, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, z.Err(err, "open")
	}

	var c Config
	if err := yaml.NewDecoder(f).Decode(&c); err != nil {
		return nil, z.Err(err, "decode")
	}

	c.path = p
	return &c, nil
}

func (c *Config) Path() string {
	return c.path
}

func (c *Config) Evaluate() error {
	if err := c.evaluateCld(); err != nil {
		return err
	}
	return c.evaluateSession()
}
