package devcup

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// RunContainerized executes `devcontainer up` inside the runner image.
// The workspace is mounted at its host path so the paths the CLI records in
// container labels match the host, and HOME is set to the host home so
// ${localEnv:HOME}-style substitutions in devcontainer.json resolve the same
// as they would on the host.
func RunContainerized(ctx context.Context, cli *client.Client, o Options) error {
	access, err := AccessFor(os.Getenv("DOCKER_HOST"))
	if err != nil {
		return err
	}
	// The runner shares the workspace with the engine by bind-mounting it at
	// the same path. That only works when the engine is local (a unix socket);
	// against a remote engine (tcp DOCKER_HOST) the bind resolves on the remote
	// host and would silently mount an empty directory. Refuse it clearly.
	if access.Bind == "" {
		return fmt.Errorf("the containerized devcontainer CLI needs a local Docker engine, " +
			"but DOCKER_HOST points at a remote one; install the devcontainer CLI or Node on " +
			"this host, or run `cld up` where the engine is")
	}

	if _, err := cli.ImageInspect(ctx, o.RunnerImage); err != nil {
		fmt.Fprintf(o.Stderr, "pulling %s...\n", o.RunnerImage)
		res, err := cli.ImagePull(ctx, o.RunnerImage, client.ImagePullOptions{})
		if err != nil {
			return fmt.Errorf("pull %s: %w", o.RunnerImage, err)
		}
		io.Copy(io.Discard, res)
		werr := res.Wait(ctx)
		res.Close()
		if werr != nil {
			return fmt.Errorf("pull %s: %w", o.RunnerImage, werr)
		}
	}

	// HOME lets ${localEnv:HOME} substitutions in devcontainer.json resolve;
	// the engine is local, so the host home is a real path there.
	env := []string{}
	if h, err := os.UserHomeDir(); err == nil {
		env = append(env, "HOME="+h)
	}

	// The workspace is bind-mounted at its host path, so a built-in default
	// written into it at .devcontainer/devcontainer.json is visible to the CLI
	// here just as it is on the host — no separate override mount needed.
	binds := []string{o.Workspace + ":" + o.Workspace, access.Bind}

	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      o.RunnerImage,
			Entrypoint: []string{"devcontainer"},
			Cmd:        o.up_args(),
			Env:        env,
			WorkingDir: o.Workspace,
		},
		HostConfig: &container.HostConfig{Binds: binds},
	})
	if err != nil {
		return fmt.Errorf("create runner: %w", err)
	}
	id := created.ID
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	}()

	att, err := cli.ContainerAttach(ctx, id, client.ContainerAttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return fmt.Errorf("attach runner: %w", err)
	}
	defer att.Close()

	if _, err := cli.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start runner: %w", err)
	}

	stdcopy.StdCopy(o.Stdout, o.Stderr, att.Reader)

	wait := cli.ContainerWait(ctx, id, client.ContainerWaitOptions{})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-wait.Error:
		return fmt.Errorf("wait runner: %w", err)
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			return fmt.Errorf("devcontainer up exited with %d", res.StatusCode)
		}
	}
	return nil
}
