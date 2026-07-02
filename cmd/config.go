package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/goccy/go-yaml"
	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/z"
)

var use_config = z.NewUse[*config.Config]()

func NewCmdConfig() *xli.Command {
	return &xli.Command{
		Name:  "config",
		Brief: "print current configuration as YAML",
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			return yaml.NewEncoder(cmd).Encode(c)
		}),
	}
}

func UseConfigInit(ctx context.Context, cmd *xli.Command) (context.Context, *config.Config, error) {
	if _, ok := use_config.From(ctx); ok {
		return nil, nil, fmt.Errorf("config already in context")
	}

	var (
		c   *config.Config
		err error
	)
	if p, ok := flg.Find[string](cmd, "config"); ok {
		c, err = config.ReadFromFile(p)
		if err != nil {
			return nil, nil, z.Err(err, "read config")
		}
	} else {
		for _, p := range config.DefaultConfigPaths {
			c, err = config.ReadFromFile(p)
			if err == nil {
				break
			}
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, nil, z.Err(err, "read config: %q", p)
		}
		if c == nil {
			c = &config.Config{}
		}
	}

	if err := c.Evaluate(); err != nil {
		return nil, nil, z.Err(err, "evaluate config")
	}

	ctx = use_config.Into(ctx, c)
	return ctx, c, nil
}
