// Package devcup runs `devcontainer up` for a workspace, preferring a CLI
// already on the host and falling back to running the official CLI in a
// container — so `cld up` works with nothing but Docker installed.
package devcup

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type Options struct {
	// Workspace is the absolute host path of the project.
	Workspace string
	// Args are extra arguments appended to `devcontainer up`.
	Args []string
	// RunnerImage runs the CLI when no `devcontainer` binary is on the host.
	RunnerImage string

	Stdout io.Writer
	Stderr io.Writer
}

func (o *Options) up_args() []string {
	return append([]string{"up", "--workspace-folder", o.Workspace}, o.Args...)
}

// HasConfig reports whether the workspace has any devcontainer configuration —
// one of the two standard locations or a sub-folder config under .devcontainer/.
func HasConfig(workspace string) bool {
	return len(DiscoverConfigs(workspace)) > 0
}

// Config is a devcontainer configuration discovered in a workspace.
type Config struct {
	// Path is the config file's absolute host path.
	Path string
	// Rel is Path relative to the workspace, e.g. ".devcontainer/devcontainer.json"
	// or ".devcontainer/go/devcontainer.json".
	Rel string
	// Label is a short human name for pickers: "default" / ".devcontainer.json"
	// for the standard locations, or the sub-folder name for the rest.
	Label string
	// Standard is true for the two locations the devcontainer CLI auto-detects
	// (.devcontainer/devcontainer.json and .devcontainer.json); a false value
	// means the config must be passed to the CLI with --config.
	Standard bool
}

// DiscoverConfigs enumerates every devcontainer.json in the workspace the
// devcontainer spec recognizes, in a stable order:
//
//  1. .devcontainer/devcontainer.json   (primary standard location)
//  2. .devcontainer.json                (root standard location)
//  3. .devcontainer/<folder>/devcontainer.json (sub-folder configs, sorted)
//
// The two standard locations are auto-detected by the CLI; the sub-folder
// configs exist in the spec but need an explicit --config, which is why each
// result carries Standard and its Rel path.
func DiscoverConfigs(workspace string) []Config {
	var out []Config
	add := func(rel, label string, standard bool) {
		p := filepath.Join(workspace, rel)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			out = append(out, Config{Path: p, Rel: rel, Label: label, Standard: standard})
		}
	}

	add(filepath.Join(".devcontainer", "devcontainer.json"), "default", true)
	add(".devcontainer.json", ".devcontainer.json", true)

	// Sub-folder configs live exactly one level under .devcontainer/, as
	// .devcontainer/<folder>/devcontainer.json. Enumerate them sorted by folder
	// so the picker order is deterministic.
	entries, _ := os.ReadDir(filepath.Join(workspace, ".devcontainer"))
	var folders []string
	for _, e := range entries {
		if e.IsDir() {
			folders = append(folders, e.Name())
		}
	}
	sort.Strings(folders)
	for _, name := range folders {
		add(filepath.Join(".devcontainer", name, "devcontainer.json"), name, false)
	}
	return out
}

// Runner is one way of executing the devcontainer CLI.
type Runner struct {
	// Desc is shown to the user before running.
	Desc string
	Run  func(ctx context.Context) error
}

// Resolve picks how to run the CLI: a devcontainer binary on PATH, else the
// runner image. look_path is a parameter for tests; pass exec.LookPath.
func Resolve(o Options, look_path func(string) (string, error), containerized func(ctx context.Context) error) Runner {
	if p, err := look_path("devcontainer"); err == nil {
		return Runner{
			Desc: "devcontainer CLI at " + p,
			Run: func(ctx context.Context) error {
				return run_command(ctx, o, p, o.up_args()...)
			},
		}
	}
	return Runner{
		Desc: "containerized devcontainer CLI (" + o.RunnerImage + ")",
		Run:  containerized,
	}
}

func run_command(ctx context.Context, o Options, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = o.Workspace
	cmd.Stdout = o.Stdout
	cmd.Stderr = o.Stderr
	return cmd.Run()
}

// DockerAccess describes how the runner container reaches the Docker daemon,
// derived from the client's DOCKER_HOST.
type DockerAccess struct {
	// Bind mounts the host socket into the runner at the default path.
	Bind string // "<host path>:/var/run/docker.sock", or ""
	// Env carries DOCKER_HOST through for TCP daemons, or "" for the default
	// socket path.
	Env string
}

// AccessFor derives the runner's daemon access from a DOCKER_HOST value.
func AccessFor(docker_host string) (DockerAccess, error) {
	switch {
	case docker_host == "":
		return DockerAccess{Bind: "/var/run/docker.sock:/var/run/docker.sock"}, nil
	case strings.HasPrefix(docker_host, "unix://"):
		p := strings.TrimPrefix(docker_host, "unix://")
		return DockerAccess{Bind: p + ":/var/run/docker.sock"}, nil
	case strings.HasPrefix(docker_host, "tcp://"):
		return DockerAccess{Env: "DOCKER_HOST=" + docker_host}, nil
	default:
		return DockerAccess{}, fmt.Errorf("unsupported DOCKER_HOST %q", docker_host)
	}
}
