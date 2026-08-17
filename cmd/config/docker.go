package config

import "fmt"

// This file holds the per-session Docker engine policy: whether cld gives a
// project's claude session an engine of its own, and what to run it from. See
// docs/session-docker.md — including why enabling it is a risk the user takes
// on deliberately.

const (
	// DockerModeOff is the default: cld does nothing about Docker, and a
	// session sees whatever the devcontainer itself arranged.
	DockerModeOff = "off"
	// DockerModeDind gives the project its own docker-in-docker engine on a
	// private network, with DOCKER_HOST pointed at it.
	DockerModeDind = "dind"
)

// DockerImageDefault is the engine image. The moving tag is deliberate: it is
// the one that always exists. Pin it (e.g. "docker:28-dind") when you would
// rather control when the engine changes under you.
const DockerImageDefault = "docker:dind"

// DockerEnginePort is the port the engine listens on, without TLS, on the
// private network cld creates for it. Plaintext is acceptable only because the
// network holds exactly two containers — the engine and the devcontainer it
// belongs to — and anything on it can control the engine anyway.
const DockerEnginePort = 2375

// DockerConfig declares whether a session gets a Docker engine. An empty field
// means "not specified here", so a project block can override one field
// without restating the others.
type DockerConfig struct {
	// Mode is DockerModeOff (the default) or DockerModeDind.
	Mode string `yaml:"mode"`
	// Image the engine runs from; defaults to DockerImageDefault. Point it at
	// docker:dind-rootless to trade capability for a smaller blast radius.
	Image string `yaml:"image"`
}

// Enabled reports whether cld should provide an engine.
func (c DockerConfig) Enabled() bool { return c.Mode == DockerModeDind }

// DockerFor resolves the engine policy for a workspace: the global block, then
// every matching project block in file order, field by field. Defaults are
// applied last, so the result is always complete.
func (c *Config) DockerFor(local_folder string) DockerConfig {
	out := c.Docker
	for _, p := range c.MatchProjects(local_folder) {
		if p.Docker.Mode != "" {
			out.Mode = p.Docker.Mode
		}
		if p.Docker.Image != "" {
			out.Image = p.Docker.Image
		}
	}
	if out.Mode == "" {
		out.Mode = DockerModeOff
	}
	if out.Image == "" {
		out.Image = DockerImageDefault
	}
	return out
}

func validateDocker(d DockerConfig, where string) error {
	switch d.Mode {
	case "", DockerModeOff, DockerModeDind:
	default:
		return fmt.Errorf("%s: mode %q: want %q or %q", where, d.Mode, DockerModeOff, DockerModeDind)
	}
	return nil
}
