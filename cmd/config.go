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
	"github.com/lesomnus/cld/internal/tui"
	"github.com/lesomnus/xli"
	"github.com/lesomnus/xli/arg"
	"github.com/lesomnus/xli/flg"
	"github.com/lesomnus/xli/tab"
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
		Brief: "open cld's config (cld.yaml, or the engine override) in $EDITOR",
		Args: arg.Args{
			&arg.String{
				Name:     "file",
				Brief:    "which file: `cld` (default) or `dind` (the engine override)",
				Optional: true,
				Handler:  completeConfigEditTargets(),
			},
		},
		Handler: xli.OnRun(func(ctx context.Context, cmd *xli.Command, next xli.Next) error {
			c := use_config.Must(ctx)

			which, _ := arg.Get[string](cmd, "file")
			switch which {
			case "", "cld", "cld.yaml", "config":
				return editConfigFile(ctx, cmd, configEditPath(c))
			case "dind", "docker", "engine":
				p := c.DindOverrideEditPath()
				if p == "" {
					return fmt.Errorf("cannot resolve where the engine override lives; pass --config")
				}
				return editDindFile(ctx, cmd, p)
			default:
				return fmt.Errorf("unknown file %q: use `cld` or `dind`", which)
			}
		}),
	}
}

func completeConfigEditTargets() arg.Handler[string] {
	return arg.OnTab[string](func(ctx context.Context, t tab.Tab) {
		t.ValueD("cld", "cld.yaml — cld's own configuration")
		t.ValueD("dind", "cld.dind.yaml — overrides for the Docker engine cld runs")
	})
}

// dindConfigTemplate seeds a fresh engine override. It shows the shape (a
// compose-style service named dind) and the one rule that is not guessable —
// volume sources are host paths — since an empty file would not parse: the
// document has to carry that service.
var dindConfigTemplate = []byte(
	"# Overrides for the Docker engine cld runs for sessions.\n" +
		"# Compose-shaped, but only the keys that map onto a container are\n" +
		"# supported; anything else is rejected when this file is read.\n" +
		"# See docs/session-docker.md.\n" +
		"services:\n" +
		"  dind:\n" +
		"    # volumes:\n" +
		"    #   - /srv/build-cache:/cache   # HOST paths; ~/ and ${HOME}/ expand\n" +
		"    # command: [\"--insecure-registry\", \"registry.internal:5000\"]\n" +
		"    # mem_limit: 8g\n")

// configVisibilityNote warns when the file about to be edited is not the one
// the daemon reads, which is a quiet failure the user only discovers when a
// setting never takes. There are two ways to land there and both are easy: the
// daemon runs on the host, so nothing inside a devcontainer is its config; and
// a cld.yaml in the working directory wins over the user's, so editing inside
// any checkout that has one writes there.
//
// Returns "" when the target is the daemon's own config directory.
func configVisibilityNote(path, daemon_dir string, in_devcontainer bool) string {
	if in_devcontainer {
		return "you are inside a devcontainer, so this file is client-only; the daemon " +
			"reads ~/" + config.UserConfigDirName + "/ on the HOST — edit it there"
	}
	if daemon_dir == "" || filepath.Dir(path) == daemon_dir {
		return ""
	}
	return "the daemon reads " + daemon_dir + "/; this file is client-only " +
		"unless the daemon was pointed at it"
}

// warnConfigVisibility prints that note, if there is one, before the editor
// opens — so it is seen whether or not the edit is saved.
func warnConfigVisibility(cmd *xli.Command, path string) {
	// The daemon's own container has CLD_HOST_HOME set, and there the user
	// config dir does resolve to the mounted host home; a managed devcontainer
	// is the case that cannot see the daemon's filesystem at all.
	in_devcontainer := insideContainer() && os.Getenv(config.HostHomeEnv) == ""
	if note := configVisibilityNote(path, config.UserConfigDir(), in_devcontainer); note != "" {
		fmt.Fprintf(cmd.ErrWriter, "%s: note: %s\n", tui.Tag(), note)
	}
}

// editDindFile opens the engine override, rejecting a save cld could not act
// on — an unknown key or a relative volume source is caught here rather than
// silently failing to apply at the next provisioning.
func editDindFile(ctx context.Context, cmd *xli.Command, path string) error {
	warnConfigVisibility(cmd, path)

	orig, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		orig = dindConfigTemplate
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dir, err)
			}
		}
	} else if err != nil {
		return err
	}

	validate := func(b []byte) error {
		_, err := config.ParseDindFile(b, "")
		return err
	}
	return editInEditor(ctx, cmd, path, orig, ".yaml", validate, "is not a usable engine override", func() {
		fmt.Fprintln(cmd.ErrWriter, "cld: restart the daemon to load it; the engine is replaced on the next provisioning")
	})
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
	warnConfigVisibility(cmd, path)

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
