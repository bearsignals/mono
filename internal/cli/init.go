package cli

import (
	"fmt"
	"os"

	"github.com/gwuah/mono/internal/mono"
	"github.com/spf13/cobra"
)

func NewInitCmd() *cobra.Command {
	var projectRoot string

	cmd := &cobra.Command{
		Use:   "init [path]",
		Short: "Initialize a new environment",
		Long:  "Register an environment, start containers, and create a tmux session.\nIf no path is provided, uses CONDUCTOR_WORKSPACE_PATH.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			absPath, err := resolvePath(args)
			if err != nil {
				return err
			}

			if _, err := os.Stat(absPath); err != nil {
				return fmt.Errorf("path does not exist: %s", absPath)
			}

			return mono.Init(absPath, projectRoot)
		},
	}

	cmd.Flags().StringVar(&projectRoot, "project", "", "root path of the project (falls back to CONDUCTOR_ROOT_PATH)")

	return cmd
}
