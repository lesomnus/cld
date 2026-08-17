package config

import "fmt"

// This file holds the per-session Docker engine policy: whether cld gives a
// project's claude session an engine of its own, and what to run it from. See
// docs/session-docker.md — including the risk that comes with it, which is now
// taken on by default and has to be opted out of.

const (
	// DockerModeOff leaves Docker alone: a session sees whatever the
	// devcontainer itself arranged, and cld runs nothing extra.
	DockerModeOff = "off"
	// DockerModeDind gives the project its own docker-in-docker engine on a
	// private network, with DOCKER_HOST pointed at it.
	DockerModeDind = "dind"
)

// DockerModeDefault is what a project gets when cld.yaml says nothing about
// Docker: an engine of its own.
//
// It is not free, and both costs are worth stating. Every managed project runs
// one extra PRIVILEGED container that shares the host kernel — the session, and
// so claude and anything claude runs, is root on it — and the first
// provisioning on a host pulls the engine image. Turn it off globally, or for
// one project, with `docker: {mode: off}`. See docs/session-docker.md.
const DockerModeDefault = DockerModeDind

// DockerImageDefault is the engine image. The moving tag is deliberate: it is
// the one that always exists. Pin it (e.g. "docker:28-dind") when you would
// rather control when the engine changes under you.
const DockerImageDefault = "docker:dind"

// DockerLabel marks every container and network cld creates for an engine,
// valued with the key naming that engine. It lives here rather than with the
// daemon so `cld uninstall` can find them too: an engine outliving cld would
// leave a privileged container running for nothing.
const DockerLabel = "cld.dind"

// DockerEnginePort is the port the engine listens on, without TLS, on the
// private network cld creates for it. Plaintext is acceptable only because
// nothing but cld's own devcontainers is on that network, and any of them can
// control the engine anyway.
const DockerEnginePort = 2375

const (
	// DockerScopeShared runs ONE engine for every project that uses it. The
	// point is what an engine accumulates: a single BuildKit cache and one
	// daemon's memory, instead of a copy per project.
	//
	// The cost is that no workspace can be bind-mounted into it — mounts are
	// fixed when a container is created, so a shared engine would have to be
	// recreated (killing everything running in it) each time a new project
	// appeared. Builds are unaffected: a build context is streamed by the
	// client, not read from the engine's filesystem. `docker run -v <workspace>`
	// is what does not work; use DockerScopeProject when a project needs it.
	DockerScopeShared = "shared"
	// DockerScopeProject gives the project an engine of its own, with its
	// workspace bound at the path the devcontainer uses — so `docker run -v
	// $(pwd):/app` means what it looks like. Its cache is its own.
	DockerScopeProject = "project"
)

// DockerScopeDefault shares one engine, which is the point of running one at
// all: a build cache that is warm because every project fills it.
const DockerScopeDefault = DockerScopeShared

// DockerConfig declares whether a session gets a Docker engine. An empty field
// means "not specified here", so a project block can override one field
// without restating the others.
type DockerConfig struct {
	// Mode is DockerModeDind (the default) or DockerModeOff.
	Mode string `yaml:"mode"`
	// Scope is DockerScopeShared (the default) or DockerScopeProject.
	Scope string `yaml:"scope"`
	// Image the engine runs from; defaults to DockerImageDefault. Point it at
	// docker:dind-rootless to trade capability for a smaller blast radius.
	Image string `yaml:"image"`
	// Compose names a compose-shaped file that overrides the engine container
	// cld would otherwise create — extra volumes, dockerd flags, capabilities.
	// Relative paths resolve against the config directory. Defaults to
	// DindFileName there, used when it exists; naming one explicitly makes it
	// required. See dind_file.go.
	Compose string `yaml:"compose"`
}

// Enabled reports whether cld should provide an engine.
func (c DockerConfig) Enabled() bool { return c.Mode == DockerModeDind }

// Shared reports whether this project uses the one engine every project shares,
// rather than an engine of its own.
func (c DockerConfig) Shared() bool { return c.Scope != DockerScopeProject }

// DockerFor resolves the engine policy for a workspace: the global block, then
// every matching project block in file order, field by field. Defaults are
// applied last, so the result is always complete.
func (c *Config) DockerFor(local_folder string) DockerConfig {
	out := c.Docker
	for _, p := range c.MatchProjects(local_folder) {
		if p.Docker.Mode != "" {
			out.Mode = p.Docker.Mode
		}
		if p.Docker.Scope != "" {
			out.Scope = p.Docker.Scope
		}
		if p.Docker.Image != "" {
			out.Image = p.Docker.Image
		}
		if p.Docker.Compose != "" {
			out.Compose = p.Docker.Compose
		}
	}
	if out.Mode == "" {
		out.Mode = DockerModeDefault
	}
	if out.Scope == "" {
		out.Scope = DockerScopeDefault
	}
	// A project may choose whether to use the shared engine, but not reshape
	// it: it belongs to every other project too. Were two projects to resolve
	// different images or overrides for it, each reconcile would rebuild it out
	// from under the other, forever. Config rejects the combination at load —
	// this makes it true even for a Config built in code.
	if out.Shared() {
		out.Image, out.Compose = c.Docker.Image, c.Docker.Compose
	}
	if out.Image == "" {
		out.Image = DockerImageDefault
	}
	return out
}

// validateProjectDocker adds the rule that only applies inside a project block:
// the engine-shaping fields are meaningless — and would be silently dropped —
// unless the project has an engine of its own. global is the top-level scope,
// which the block inherits when it does not set one.
func validateProjectDocker(d DockerConfig, global string, where string) error {
	if err := validateDocker(d, where); err != nil {
		return err
	}

	scope := d.Scope
	if scope == "" {
		scope = global
	}
	if scope == "" {
		scope = DockerScopeDefault
	}
	if scope != DockerScopeShared {
		return nil
	}
	for field, v := range map[string]string{"image": d.Image, "compose": d.Compose} {
		if v == "" {
			continue
		}
		return fmt.Errorf("%s: %s shapes the engine, which this project shares with the others; "+
			"add `scope: %s` to give it one of its own, or set %s at the top level for everyone",
			where, field, DockerScopeProject, field)
	}
	return nil
}

func validateDocker(d DockerConfig, where string) error {
	switch d.Mode {
	case "", DockerModeOff, DockerModeDind:
	default:
		return fmt.Errorf("%s: mode %q: want %q or %q", where, d.Mode, DockerModeOff, DockerModeDind)
	}
	switch d.Scope {
	case "", DockerScopeShared, DockerScopeProject:
	default:
		return fmt.Errorf("%s: scope %q: want %q or %q", where, d.Scope, DockerScopeShared, DockerScopeProject)
	}
	return nil
}
