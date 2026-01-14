package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func resolvePath(args []string) (string, error) {
	var path string
	if len(args) > 0 && args[0] != "" {
		path = args[0]
	} else if envPath := os.Getenv("CONDUCTOR_WORKSPACE_PATH"); envPath != "" {
		path = envPath
	} else {
		return "", fmt.Errorf("no path provided and CONDUCTOR_WORKSPACE_PATH not set")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path: %w", err)
	}

	return absPath, nil
}

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

	return cmd
}
