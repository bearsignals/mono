package cli

import (
	"github.com/spf13/cobra"
)

func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mono",
		Short: "Runtime backend for Conductor workspaces",
		Long:  "mono manages execution environments for Conductor workspaces - Docker containers, tmux sessions, and data directories.",
	}

	cmd.AddCommand(NewInitCmd())
	cmd.AddCommand(NewDestroyCmd())
	cmd.AddCommand(NewRunCmd())
	cmd.AddCommand(NewListCmd())
	cmd.AddCommand(NewSyncCmd())
	cmd.AddCommand(NewCacheCmd())

	return cmd
}
