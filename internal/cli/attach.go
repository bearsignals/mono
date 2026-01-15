package cli

import (
	"os"

	"github.com/gwuah/mono/internal/mono"
	"github.com/spf13/cobra"
)

func NewAttachCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Attach to a tmux session",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			return mono.Attach(cwd)
		},
	}
	return cmd
}
