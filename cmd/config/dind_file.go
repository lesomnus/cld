package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"
)

// This file holds the optional override for the engine container cld creates
// (see docker.go): a compose-shaped fragment the user drops next to cld.yaml.
//
// It is compose-SHAPED, not compose. cld creates the engine through the Docker
// API — the daemon has no compose CLI and shells out to nothing — so what is
// supported is the set of service keys that map onto a container, listed in
// DindService. Anything else is rejected when the file is read, which is far
// kinder than accepting a `healthcheck:` and silently ignoring it.

// DindFileName is the override read from the config directory when
// DockerConfig.Compose is not set.
const DindFileName = "cld.dind.yaml"

// DindServiceName is the service cld reads out of the file. The file is a
// mapping of services so it looks like — and can be opened as — a compose file.
const DindServiceName = "dind"

// DindFile is the whole override document.
type DindFile struct {
	Services map[string]DindService `yaml:"services"`
}

// DindService is the supported subset of a compose service, as it applies to a
// Docker engine container. Every field is optional; an unset one leaves cld's
// own value alone.
//
// Scalars replace, maps merge key by key, and lists are appended to what cld
// already sets — so a volume or a capability adds to the engine rather than
// replacing its storage or its privileges.
type DindService struct {
	// Image replaces docker.image for this engine.
	Image string `yaml:"image"`
	// Command is passed to the engine's entrypoint: dockerd flags, e.g.
	// ["--insecure-registry", "registry.internal:5000"].
	Command StringList `yaml:"command"`
	// Entrypoint replaces the image's own.
	Entrypoint StringList `yaml:"entrypoint"`
	// Environment merges into the engine's environment.
	Environment EnvList `yaml:"environment"`
	// Volumes are appended. They are paths on the HOST as the engine resolves
	// them, so they must be absolute: the daemon sees your home only through
	// its own read-only mount and cannot expand "~" to a host path.
	Volumes []string `yaml:"volumes"`
	// Privileged turns the privileged flag off (or back on) — the one knob for
	// running a rootless engine image.
	Privileged  *bool             `yaml:"privileged"`
	CapAdd      []string          `yaml:"cap_add"`
	CapDrop     []string          `yaml:"cap_drop"`
	Devices     []string          `yaml:"devices"`
	Sysctls     map[string]string `yaml:"sysctls"`
	SecurityOpt []string          `yaml:"security_opt"`
	ExtraHosts  []string          `yaml:"extra_hosts"`
	Dns         []string          `yaml:"dns"`
	// Ports publishes the engine on the host, e.g. ["12375:2375"]. Consider
	// what that exposes before using it: the engine has no TLS and no auth.
	Ports  []string          `yaml:"ports"`
	Labels map[string]string `yaml:"labels"`
	// MemLimit is a size like "4g"; Cpus is a core count like 2.5.
	MemLimit string  `yaml:"mem_limit"`
	Cpus     float64 `yaml:"cpus"`
}

// EnvList is an environment written either as a mapping or as a list of
// KEY=VALUE strings, as compose allows both.
type EnvList map[string]string

func (e *EnvList) UnmarshalYAML(b []byte) error {
	var m map[string]string
	if err := yaml.Unmarshal(b, &m); err == nil {
		*e = m
		return nil
	}

	var list []string
	if err := yaml.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("want a mapping or a list of KEY=VALUE")
	}
	out := EnvList{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("%q: want KEY=VALUE", kv)
		}
		out[k] = v
	}
	*e = out
	return nil
}

// DindOverridePath is the file to read for a workspace, or "" when the user has
// not put one there. A relative DockerConfig.Compose resolves against the
// config directory, so the override travels with cld.yaml.
func (c *Config) DindOverridePath(local_folder string) string {
	name := c.DockerFor(local_folder).Compose
	explicit := name != ""
	if !explicit {
		name = DindFileName
	}

	p := name
	if !filepath.IsAbs(p) {
		dir := c.Dir()
		if dir == "" {
			return ""
		}
		p = filepath.Join(dir, name)
	}

	if _, err := os.Stat(p); err != nil {
		if explicit {
			// An override the user asked for by name must not be silently
			// skipped when it is missing; LoadDindOverride reports it.
			return p
		}
		return ""
	}
	return p
}

// Dir is the directory cld's config was loaded from, which is where a file
// named next to it is looked for. It falls back to the user config directory
// when no file was loaded, so the convention holds even before cld.yaml exists.
func (c *Config) Dir() string {
	if c.path != "" {
		return filepath.Dir(c.path)
	}
	return UserConfigDir()
}

// LoadDindOverride reads the engine override for a workspace. It returns nil
// when there is none. Unknown keys are an error: silently dropping a key the
// user wrote is how a setting appears to be applied and is not.
func (c *Config) LoadDindOverride(local_folder string) (*DindService, error) {
	p := c.DindOverridePath(local_folder)
	if p == "" {
		return nil, nil
	}

	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: no such file", p)
		}
		return nil, err
	}
	return ParseDindFile(b, p)
}

// ParseDindFile reads an override document. where names the source in error
// messages — normally its path; pass "" when the caller already says which file
// it is, as `cld config edit dind` does while validating an unsaved buffer.
// That editor path is the point of taking bytes at all: a bad override is
// caught before it is written rather than at the next provisioning.
func ParseDindFile(b []byte, where string) (*DindService, error) {
	at := ""
	if where != "" {
		at = where + ": "
	}

	var f DindFile
	if err := yaml.UnmarshalWithOptions(b, &f, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("%s%w", at, err)
	}

	svc, ok := f.Services[DindServiceName]
	if !ok {
		return nil, fmt.Errorf("%sno %q service; the engine is configured under services.%s",
			at, DindServiceName, DindServiceName)
	}
	if err := svc.validate(at); err != nil {
		return nil, err
	}
	return &svc, nil
}

// DindOverrideEditPath is where `cld config edit dind` writes: the file
// docker.compose names, or DindFileName in the config directory. Unlike
// DindOverridePath it does not care whether the file exists yet — it is the
// path one would be created at.
func (c *Config) DindOverrideEditPath() string {
	name := c.Docker.Compose
	if name == "" {
		name = DindFileName
	}
	if filepath.IsAbs(name) {
		return name
	}
	dir := c.Dir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

// validate checks what the schema cannot. at is a message prefix, already
// punctuated ("path: ") or empty.
func (s DindService) validate(at string) error {
	for _, v := range s.Volumes {
		src, _, ok := strings.Cut(v, ":")
		if !ok {
			return fmt.Errorf("%svolume %q: want SOURCE:TARGET[:OPTIONS]", at, v)
		}
		// The engine resolves these on the host, where the daemon's own view of
		// paths does not apply.
		if !filepath.IsAbs(src) {
			return fmt.Errorf("%svolume %q: SOURCE must be an absolute host path "+
				"(a named volume or \"~\" cannot be resolved here)", at, v)
		}
	}
	if s.Cpus < 0 {
		return fmt.Errorf("%scpus must not be negative", at)
	}
	return nil
}
