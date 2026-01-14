package cli

import (
	"fmt"
	"path/filepath"

	"github.com/gwuah/mono/internal/mono"
	"github.com/spf13/cobra"
)

func NewSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync <path>",
		Short: "Sync build artifacts to cache",
		Long:  "Save current build artifacts (target/, node_modules/) to the cache for reuse.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]

			absPath, err := filepath.Abs(path)
			if err != nil {
				return fmt.Errorf("invalid path: %w", err)
			}

			db, err := mono.OpenDB()
			if err != nil {
				return fmt.Errorf("failed to open database: %w", err)
			}
			defer db.Close()

			env, err := db.GetEnvironmentByPath(absPath)
			if err != nil {
				return fmt.Errorf("environment not found: %w", err)
			}

			cfg, err := mono.LoadConfig(absPath)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			cfg.ApplyDefaults(absPath)

			cm, err := mono.NewCacheManager()
			if err != nil {
				return fmt.Errorf("failed to create cache manager: %w", err)
			}

			rootPath := ""
			if env.RootPath.Valid {
				rootPath = env.RootPath.String
			}

			if rootPath == "" {
				return fmt.Errorf("environment has no root path set")
			}

			err = cm.Sync(cfg.Build.Artifacts, rootPath, absPath, mono.SyncOptions{
				HardlinkBack: true,
			})
			if err != nil {
				return err
			}

			fmt.Println("Sync complete")
			return nil
		},
	}

	return cmd
}
