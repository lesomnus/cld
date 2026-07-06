package cmd

import (
	"context"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/daemon"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/tab"
)

// completeNames is the shell-completion handler for a devcontainer `name`
// argument (cld it / cld down): it offers the names and aliases the daemon
// currently tracks. Completion must never error or stall the shell, so the
// config load and the daemon query are best-effort under a short deadline; if
// the daemon is unreachable it simply offers nothing.
func completeNames() arg.Handler[string] {
	return arg.OnTab[string](func(ctx context.Context, t tab.Tab) {
		c := &config.Config{}
		for _, p := range config.DefaultConfigPaths {
			if cc, err := config.ReadFromFile(p); err == nil {
				c = cc
				break
			}
		}
		if err := c.Evaluate(); err != nil {
			return
		}

		ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		items, err := daemon.FetchItems(ctx, c.SocketPath())
		if err != nil {
			return
		}
		for _, it := range items {
			t.ValueD(it.Name, string(it.Status))
			// The alias resolves too (see by_name), so offer it as well, with
			// the full name as its description.
			if it.Alias != "" && it.Alias != it.Name {
				t.ValueD(it.Alias, it.Name)
			}
		}
	})
}
