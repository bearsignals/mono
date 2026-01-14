package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gwuah/mono/internal/mono"
	"github.com/spf13/cobra"
)

func NewCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage build cache",
		Long:  "View cache statistics and clean cached build artifacts.",
	}

	cmd.AddCommand(newCacheStatsCmd())
	cmd.AddCommand(newCacheCleanCmd())

	return cmd
}

func newCacheStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Show cache usage statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			cm, err := mono.NewCacheManager()
			if err != nil {
				return err
			}

			db, err := mono.OpenDB()
			if err != nil {
				return err
			}
			defer db.Close()

			sizes, err := cm.GetCacheSizes()
			if err != nil {
				return err
			}

			if len(sizes) == 0 {
				fmt.Println("No cache entries found.")
				return nil
			}

			stats, err := db.GetCacheStats()
			if err != nil {
				return err
			}

			statsMap := make(map[string]mono.CacheEntry)
			for _, s := range stats {
				key := s.ProjectID + "/" + s.Artifact + "/" + s.CacheKey
				statsMap[key] = s
			}

			fmt.Printf("%-14s %-10s %-12s %6s %8s   %s\n", "Project", "Artifact", "Key", "Hits", "Size", "Last Used")
			fmt.Println(strings.Repeat("─", 75))

			var totalSize int64
			for _, entry := range sizes {
				totalSize += entry.Size
				key := entry.ProjectID + "/" + entry.Artifact + "/" + entry.CacheKey

				hits := 0
				lastUsed := "never"
				if s, ok := statsMap[key]; ok {
					hits = s.Hits
					lastUsed = formatTimeAgo(s.LastUsed)
				}

				fmt.Printf("%-14s %-10s %-12s %6d %8s   %s\n",
					entry.ProjectID,
					entry.Artifact,
					entry.CacheKey,
					hits,
					formatSize(entry.Size),
					lastUsed,
				)
			}

			fmt.Println(strings.Repeat("─", 75))
			fmt.Printf("Total: %d entries, %s\n", len(sizes), formatSize(totalSize))

			return nil
		},
	}
}

func newCacheCleanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove cached artifacts",
		Long:  "Remove cached build artifacts. Without flags, prompts for confirmation before cleaning all.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cm, err := mono.NewCacheManager()
			if err != nil {
				return err
			}

			db, err := mono.OpenDB()
			if err != nil {
				return err
			}
			defer db.Close()

			all, err := cmd.Flags().GetBool("all")
			if err != nil {
				return err
			}

			sizes, err := cm.GetCacheSizes()
			if err != nil {
				return err
			}

			if len(sizes) == 0 {
				fmt.Println("No cache entries to clean.")
				return nil
			}

			if !all {
				var totalSize int64
				for _, entry := range sizes {
					totalSize += entry.Size
				}
				fmt.Printf("Found %d cache entries (%s).\n", len(sizes), formatSize(totalSize))
				fmt.Print("Remove all cached artifacts? [y/N]: ")

				reader := bufio.NewReader(os.Stdin)
				response, err := reader.ReadString('\n')
				if err != nil {
					return err
				}
				response = strings.TrimSpace(strings.ToLower(response))
				if response != "y" && response != "yes" {
					fmt.Println("Aborted.")
					return nil
				}
			}

			count, totalSize, err := cm.RemoveAllCache()
			if err != nil {
				return err
			}

			if err := db.DeleteAllCacheEvents(); err != nil {
				return fmt.Errorf("failed to clear cache events: %w", err)
			}

			fmt.Printf("Removed %d entries (%s)\n", count, formatSize(totalSize))
			return nil
		},
	}

	cmd.Flags().Bool("all", false, "Remove all cached entries without prompting")

	return cmd
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)

	switch {
	case bytes >= 1000*MB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(GB))
	case bytes >= 1000*KB:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func formatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}

	duration := time.Since(t)

	switch {
	case duration < time.Minute:
		return "just now"
	case duration < time.Hour:
		mins := int(duration.Minutes())
		if mins == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", mins)
	case duration < 24*time.Hour:
		hours := int(duration.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	case duration < 7*24*time.Hour:
		days := int(duration.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		weeks := int(duration.Hours() / 24 / 7)
		if weeks == 1 {
			return "1 week ago"
		}
		return fmt.Sprintf("%d weeks ago", weeks)
	}
}
