package config

import (
	"os"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/z"
)

var DefaultConfigPaths = []string{
	"cld.yaml",
	"cld.yml",
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

	Auth     AuthConfig     `yaml:"auth"`
	Release  ReleaseConfig  `yaml:"release"`
	Gh       GhConfig       `yaml:"gh"`
	Dotfiles DotfilesConfig `yaml:"dotfiles"`
	Sync     SyncConfig     `yaml:"sync"`
	Up       UpConfig       `yaml:"up"`
	Install  InstallConfig  `yaml:"install"`

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
	return c.evaluateCld()
}
