package daemon

import (
	"context"
	"os"
	"strings"

	"github.com/moby/moby/client"
)

// detect_self_container figures out whether this daemon runs inside a Docker
// container and, if so, which one, so clients can route their tmux attach
// through a `docker exec` into it instead of needing a local tmux.
//
// Detection is best-effort: the default container hostname is the short
// container ID, so it is inspected and accepted only when it prefixes the
// resolved full ID (a custom hostname that happens to name some other
// container fails that check and safely degrades to "not in a container").
// CLD_SELF_CONTAINER overrides for setups with custom hostnames.
func detect_self_container(ctx context.Context, cli *client.Client) string {
	if v := os.Getenv("CLD_SELF_CONTAINER"); v != "" {
		if insp, err := cli.ContainerInspect(ctx, v, client.ContainerInspectOptions{}); err == nil {
			return insp.Container.ID
		}
		return ""
	}

	if _, err := os.Stat("/.dockerenv"); err != nil {
		return ""
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		return ""
	}

	insp, err := cli.ContainerInspect(ctx, hostname, client.ContainerInspectOptions{})
	if err != nil || !strings.HasPrefix(insp.Container.ID, hostname) {
		return ""
	}
	return insp.Container.ID
}
