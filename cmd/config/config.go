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

	Release ReleaseConfig `yaml:"release"`
	Sync    SyncConfig    `yaml:"sync"`

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
