package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/cld/internal/devc"
	"github.com/lesomnus/cld/internal/envx"
)

// This file holds the session policy: what environment a claude session runs
// with, which files are placed in the container for it, and which scripts run
// before it starts. All of it applies to the processes cld itself starts (the
// claude pane, its split panes, and these scripts) — cld does not create the
// container, so it cannot change the container's own environment. See
// docs/session-env.md.

// EnvMap is a set of environment variable assignments. A null value removes
// the variable from the session, which is different from an empty string:
// "FOO:" sets it to empty, "FOO: null" makes it absent.
type EnvMap map[string]*string

// FileSpec places a host file or directory into the container — the way to
// carry credentials an environment variable cannot hold, such as the TLS
// material a remote Docker endpoint needs.
type FileSpec struct {
	// Src is a host path under the home directory, written as "~/...". The
	// daemon reads the host through a read-only mount of that home (see
	// HostHomeMount), so nothing outside it is readable.
	Src string `yaml:"src"`
	// Dst is the path in the container. "${HOME}" and "${CLD_WORKSPACE}" are
	// expanded.
	Dst string `yaml:"dst"`
	// Mode is the octal permission for copied files, e.g. "0600". Defaults to
	// FileModeDefault — credentials are the motivating use, so the default is
	// tight rather than convenient.
	Mode string `yaml:"mode"`
}

// FileModeDefault is the permission a placed file gets when Mode is unset.
// Directories always get the executable bit on top of it.
const FileModeDefault = 0o600

// Perm parses Mode, or returns FileModeDefault when it is unset.
func (f FileSpec) Perm() (int64, error) {
	if f.Mode == "" {
		return FileModeDefault, nil
	}
	v, err := strconv.ParseInt(f.Mode, 8, 32)
	if err != nil || v <= 0 || v > 0o7777 {
		return 0, fmt.Errorf("invalid mode %q: want an octal permission like \"0600\"", f.Mode)
	}
	return v, nil
}

// ScriptRun is a command, written either as a shell command line (run with
// `sh -c`) or as an argv list (run with no shell at all).
type ScriptRun struct {
	Shell string
	Argv  []string
}

func (r ScriptRun) IsZero() bool { return r.Shell == "" && len(r.Argv) == 0 }

// Cmd is the argv to exec in the container.
func (r ScriptRun) Cmd() []string {
	if len(r.Argv) > 0 {
		return r.Argv
	}
	return []string{"sh", "-c", r.Shell}
}

func (r *ScriptRun) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err == nil {
		*r = ScriptRun{Shell: s}
		return nil
	}
	var argv []string
	if err := yaml.Unmarshal(b, &argv); err != nil {
		return fmt.Errorf("want a shell command line or an argv list")
	}
	*r = ScriptRun{Argv: argv}
	return nil
}

func (r ScriptRun) MarshalYAML() (any, error) {
	if len(r.Argv) > 0 {
		return r.Argv, nil
	}
	return r.Shell, nil
}

// ScriptSpec is one script to run in the container. It can be written as a
// bare command line, which is the common case:
//
//	scripts:
//	  setup: sudo apt-get install -y ripgrep
//
// or as a mapping when it needs more than the defaults.
type ScriptSpec struct {
	Run ScriptRun `yaml:"run"`
	// User to run as; defaults to the container user claude runs as. "root"
	// is the usual reason to set it (installing packages).
	User string `yaml:"user"`
	// Workdir in the container; defaults to the workspace folder.
	Workdir string `yaml:"workdir"`
	// Timeout after which the script is killed, defaulting to
	// ScriptTimeoutDefault. A container's reconciles run one at a time, so a
	// script that hangs would otherwise stall everything else for it.
	Timeout Duration `yaml:"timeout"`
	// OnError is "warn" (the default — log it and carry on) or "fail" (mark
	// the container failed). A personal script must not be able to lock the
	// user out of a claude session by default.
	OnError string `yaml:"on_error"`
}

// ScriptTimeoutDefault bounds a script that never returns.
const ScriptTimeoutDefault = 5 * time.Minute

const (
	OnErrorWarn = "warn"
	OnErrorFail = "fail"
)

func (s *ScriptSpec) UnmarshalYAML(b []byte) error {
	var run ScriptRun
	if err := run.UnmarshalYAML(b); err == nil {
		*s = ScriptSpec{Run: run}
		return nil
	}
	// A mapping. The alias avoids recursing into this method.
	type spec ScriptSpec
	var v spec
	if err := yaml.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = ScriptSpec(v)
	return nil
}

// TimeoutOrDefault is the configured timeout, or ScriptTimeoutDefault when
// unset. A non-positive value is treated as unset rather than as "no time at
// all", which would make every script fail.
func (s ScriptSpec) TimeoutOrDefault() time.Duration {
	if d := s.Timeout.Std(); d > 0 {
		return d
	}
	return ScriptTimeoutDefault
}

// Fatal reports whether a failure of this script should fail provisioning.
func (s ScriptSpec) Fatal() bool { return s.OnError == OnErrorFail }

// ScriptSet is the scripts for one scope, keyed by when they run.
type ScriptSet struct {
	// Setup runs once per container, before the session is created — after
	// cld has installed claude and restored state, so a tool it installs is
	// there from claude's first prompt. It re-runs when its own definition
	// changes, not on every reconcile.
	Setup *ScriptSpec `yaml:"setup"`
	// Start runs on every container start generation, including the first.
	Start *ScriptSpec `yaml:"start"`
}

// StringList is a value written either as one string or as a list of them.
type StringList []string

func (l *StringList) UnmarshalYAML(b []byte) error {
	var s string
	if err := yaml.Unmarshal(b, &s); err == nil {
		*l = StringList{s}
		return nil
	}
	var v []string
	if err := yaml.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("want a string or a list of strings")
	}
	*l = v
	return nil
}

func (l StringList) MarshalYAML() (any, error) { return []string(l), nil }

// ProjectConfig scopes session policy to the workspaces its globs match. Every
// matching block applies, in file order — they accumulate rather than the
// first one winning, so a broad block and a narrow one compose.
type ProjectConfig struct {
	// Match holds globs matched against the host-side workspace path, with the
	// same semantics as the top-level `ignore` list: "**" crosses path
	// separators and a leading "~/" expands to the home directory.
	Match   StringList   `yaml:"match"`
	Env     EnvMap       `yaml:"env"`
	Files   []FileSpec   `yaml:"files"`
	Scripts ScriptSet    `yaml:"scripts"`
	Docker  DockerConfig `yaml:"docker"`
}

// Matches reports whether this block applies to a host-side workspace path.
func (p ProjectConfig) Matches(local_folder string) bool {
	for _, g := range p.Match {
		if devc.MatchPath(g, local_folder) {
			return true
		}
	}
	return false
}

// MatchProjects returns the project blocks that apply to a workspace, in file
// order, so a caller can layer them in that order.
func (c *Config) MatchProjects(local_folder string) []ProjectConfig {
	out := []ProjectConfig{}
	for _, p := range c.Projects {
		if p.Matches(local_folder) {
			out = append(out, p)
		}
	}
	return out
}

// ReservedEnvKeys are the variables cld manages itself. They are rejected in
// user config rather than silently overridden: cld points CLAUDE_CONFIG_DIR at
// the config it seeds, SSH_AUTH_SOCK at the agent relay, ANTHROPIC_BASE_URL at
// the broker proxy, and so on — a session where the user quietly redirected
// one of those would break in ways no error message explains.
//
// They are rejected unconditionally, not only when the feature that sets them
// is on. Whether a config file loads must not depend on daemon state.
//
// Kept in sync with daemon.session_env by TestSessionEnvKeysAreReserved.
var ReservedEnvKeys = []string{
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_ENABLE_TELEMETRY",
	"CLAUDE_CONFIG_DIR",
	"ENABLE_TOOL_SEARCH",
	"GIT_CONFIG_GLOBAL",
	"SSH_AUTH_SOCK",
}

// ReservedEnvPrefixes are reserved by prefix; cld configures claude's whole
// OpenTelemetry exporter, not a fixed set of its variables.
var ReservedEnvPrefixes = []string{"OTEL_"}

// EnvKeyReserved reports whether a variable is cld's to set.
func EnvKeyReserved(k string) bool {
	for _, r := range ReservedEnvKeys {
		if k == r {
			return true
		}
	}
	for _, p := range ReservedEnvPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// evaluateSession validates the session policy and fills in its defaults. It
// runs at config load, so a typo is reported when the file is read rather than
// when a container happens to match it.
func (c *Config) evaluateSession() error {
	if err := validateEnv(c.Env, "env"); err != nil {
		return err
	}
	if err := validateFiles(c.Files, "files"); err != nil {
		return err
	}
	if err := validateScripts(&c.Scripts, "scripts"); err != nil {
		return err
	}
	if err := validateDocker(c.Docker, "docker"); err != nil {
		return err
	}
	// Spell the engine defaults out in the loaded config, so `cld config` says
	// whether a session gets one instead of printing an empty string for the
	// setting that decides it. Resolution applies the same defaults anyway.
	if c.Docker.Mode == "" {
		c.Docker.Mode = DockerModeDefault
	}
	if c.Docker.Scope == "" {
		c.Docker.Scope = DockerScopeDefault
	}
	if c.Docker.Image == "" {
		c.Docker.Image = DockerImageDefault
	}
	for i := range c.Projects {
		p := &c.Projects[i]
		where := fmt.Sprintf("projects[%d]", i)
		if len(p.Match) == 0 {
			return fmt.Errorf("%s: match is required", where)
		}
		for _, g := range p.Match {
			if strings.TrimSpace(g) == "" {
				return fmt.Errorf("%s: match holds an empty glob", where)
			}
		}
		if err := validateEnv(p.Env, where+".env"); err != nil {
			return err
		}
		if err := validateFiles(p.Files, where+".files"); err != nil {
			return err
		}
		if err := validateScripts(&p.Scripts, where+".scripts"); err != nil {
			return err
		}
		if err := validateProjectDocker(p.Docker, c.Docker.Scope, where+".docker"); err != nil {
			return err
		}
	}
	return nil
}

func validateEnv(m EnvMap, where string) error {
	for k := range m {
		if !envx.ValidKey(k) {
			return fmt.Errorf("%s: %q is not a valid environment variable name", where, k)
		}
		if EnvKeyReserved(k) {
			return fmt.Errorf("%s: %s is set by cld and cannot be overridden", where, k)
		}
	}
	return nil
}

func validateFiles(fs []FileSpec, where string) error {
	for i, f := range fs {
		at := fmt.Sprintf("%s[%d]", where, i)
		// The daemon reads the host through a read-only mount of the home
		// directory, so "~/..." is both the only readable source and an
		// unambiguous way to write one — an absolute path would mean different
		// things to a host daemon and a containerized one.
		if !strings.HasPrefix(f.Src, "~/") {
			return fmt.Errorf("%s: src %q must be a path under the home directory, written as \"~/...\"", at, f.Src)
		}
		if strings.Contains(f.Src, "..") {
			return fmt.Errorf("%s: src %q must not contain \"..\"", at, f.Src)
		}
		if f.Dst == "" {
			return fmt.Errorf("%s: dst is required", at)
		}
		if !strings.HasPrefix(f.Dst, "/") && !strings.HasPrefix(f.Dst, "${") {
			return fmt.Errorf("%s: dst %q must be an absolute container path", at, f.Dst)
		}
		if _, err := f.Perm(); err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
	}
	return nil
}

func validateScripts(s *ScriptSet, where string) error {
	for name, spec := range map[string]*ScriptSpec{"setup": s.Setup, "start": s.Start} {
		if spec == nil {
			continue
		}
		at := where + "." + name
		if spec.Run.IsZero() {
			return fmt.Errorf("%s: run is required", at)
		}
		switch spec.OnError {
		case "", OnErrorWarn, OnErrorFail:
		default:
			return fmt.Errorf("%s: on_error %q: want %q or %q", at, spec.OnError, OnErrorWarn, OnErrorFail)
		}
	}
	return nil
}
