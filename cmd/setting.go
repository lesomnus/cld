package cmd

import "github.com/lesomnus/xli"

// NewCmdSetting groups the commands that manage the Claude Code config cld
// installs into devcontainers: `edit` changes the shared user-default source,
// `cat` prints one container's effective config. It is deliberately separate
// from `cld config` (cld's own daemon configuration, printed as YAML).
func NewCmdSetting() *xli.Command {
	return &xli.Command{
		Name:     "setting",
		Brief:    "view and edit the Claude Code config cld installs into devcontainers",
		Commands: []*xli.Command{new_cmd_settings_edit(), new_cmd_settings_cat()},
		Handler:  xli.RequireSubcommand(),
	}
}
