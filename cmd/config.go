package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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
		// Bare `cld config` prints (OnRun fires only when this is the terminal
		// command); `cld config edit` opens the file. The `edit` subcommand
		// leaves the parent in Run|Pass mode, so the print handler stays quiet.
		Commands: []*xli.Command{new_cmd_config_edit()},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)
			return yaml.NewEncoder(cmd).Encode(c)
		}),
	}
}

func new_cmd_config_edit() *xli.Command {
	return &xli.Command{
		Name:  "edit",
		Brief: "open cld's own config (cld.yaml) in $EDITOR",
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			return editConfigFile(ctx, cmd, configEditPath(use_config.Must(ctx)))
		}),
	}
}

// configEditPath decides which file `cld config edit` opens: the file the
// running config was loaded from (which already reflects a --config path or a
// discovered cld.yaml), or, when there is none yet, the one in the user's
// config directory — created on save.
//
// A new file goes to the user config dir rather than the working directory
// because that is the only place the containerized daemon can read it too; a
// cwd cld.yaml would be edited happily and then never reach the daemon. It
// never resolves to a phantom path a bare `cld config` would not use.
func configEditPath(c *config.Config) string {
	if p := c.Path(); p != "" {
		return p
	}
	if dir := config.UserConfigDir(); dir != "" {
		return filepath.Join(dir, "cld.yaml")
	}
	return config.DefaultConfigPaths()[0]
}

// cldConfigTemplate seeds a fresh cld.yaml. It is deliberately minimal — a
// pointer to `cld config` for the full effective values rather than every
// default spelled out — so a first edit starts from a clean, commented file.
var cldConfigTemplate = []byte(
	"# cld configuration.\n" +
		"# Run `cld config` to print the effective config (defaults included),\n" +
		"# then set only the keys you want to override below.\n")

// editConfigFile opens cld's own config in the editor, seeding a template when
// it does not exist yet and rejecting a save that does not parse as YAML.
func editConfigFile(ctx context.Context, cmd *xli.Command, path string) error {
	orig, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		orig = cldConfigTemplate
		// A first edit creates the config directory: with no daemon-visible
		// config yet, this is the file the user is about to write.
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dir, err)
			}
		}
	} else if err != nil {
		return err
	}
	return editInEditor(ctx, cmd, path, orig, ".yaml", validCldConfig, "is not valid YAML", func() {
		fmt.Fprintln(cmd.ErrWriter, "cld: client commands read it immediately; restart the daemon (`cld serve`) for daemon-side changes to apply")
	})
}

// validCldConfig reports whether b parses as cld's YAML config, so a syntax
// error is caught before the file is saved rather than at the next `cld` run.
func validCldConfig(b []byte) error {
	var c config.Config
	return yaml.Unmarshal(b, &c)
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
		for _, p := range config.DefaultConfigPaths() {
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
