package cmd

import "github.com/lesomnus/xli"

// NewCmdSetting groups the commands that show what a devcontainer's claude
// actually runs with: `edit` changes the shared user-default config source,
// `cat` prints one container's effective Claude Code config, and `env` prints
// the environment its session runs with. It is deliberately separate from
// `cld config` (cld's own daemon configuration, printed as YAML).
func NewCmdSetting() *xli.Command {
	return &xli.Command{
		Name:     "setting",
		Brief:    "view and edit what cld installs into devcontainers",
		Commands: []*xli.Command{new_cmd_settings_edit(), new_cmd_settings_cat(), new_cmd_settings_env()},
		Handler:  xli.RequireSubcommand(),
	}
}
