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
	"strings"
)

type Options struct {
	// Workspace is the absolute host path of the project.
	Workspace string
	// Args are extra arguments appended to `devcontainer up`.
	Args []string
	// RunnerImage runs the CLI when neither `devcontainer` nor `npx` exists.
	RunnerImage string

	Stdout io.Writer
	Stderr io.Writer
}

func (o *Options) up_args() []string {
	return append([]string{"up", "--workspace-folder", o.Workspace}, o.Args...)
}

// HasConfig reports whether the workspace has a devcontainer configuration at
// one of the two standard locations. (Sub-folder configs under .devcontainer/
// exist in the spec but need an explicit --config, which callers can pass via
// Args.)
func HasConfig(workspace string) bool {
	for _, p := range []string{
		workspace + "/.devcontainer/devcontainer.json",
		workspace + "/.devcontainer.json",
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// Runner is one way of executing the devcontainer CLI.
type Runner struct {
	// Desc is shown to the user before running.
	Desc string
	Run  func(ctx context.Context) error
}

// Resolve picks how to run the CLI: a devcontainer binary on PATH, then npx
// (which downloads the CLI from npm on first use), then the runner image.
// look_path is a parameter for tests; pass exec.LookPath.
func Resolve(o Options, look_path func(string) (string, error), containerized func(ctx context.Context) error) Runner {
	if p, err := look_path("devcontainer"); err == nil {
		return Runner{
			Desc: "devcontainer CLI at " + p,
			Run: func(ctx context.Context) error {
				return run_command(ctx, o, p, o.up_args()...)
			},
		}
	}
	if p, err := look_path("npx"); err == nil {
		return Runner{
			Desc: "npx @devcontainers/cli (downloads on first use)",
			Run: func(ctx context.Context) error {
				args := append([]string{"--yes", "@devcontainers/cli"}, o.up_args()...)
				return run_command(ctx, o, p, args...)
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
