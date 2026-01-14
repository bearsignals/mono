package cli

import (
	"fmt"
	"os"
	"os/exec"
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

			rootPaths, err := db.GetAllRootPaths()
			if err != nil {
				return err
			}

			projectNames := buildProjectNameMap(rootPaths)

			statsMap := make(map[string]mono.CacheEntry)
			for _, s := range stats {
				key := s.ProjectID + "/" + s.Artifact + "/" + s.CacheKey
				statsMap[key] = s
			}

			fmt.Printf("%-20s %-10s %-12s %6s %8s   %s\n", "Project", "Artifact", "Key", "Hits", "Size", "Last Used")
			fmt.Println(strings.Repeat("─", 80))

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

				projectName := entry.ProjectID
				if name, ok := projectNames[entry.ProjectID]; ok {
					projectName = name
				}

				fmt.Printf("%-20s %-10s %-12s %6d %8s   %s\n",
					projectName,
					entry.Artifact,
					entry.CacheKey,
					hits,
					formatSize(entry.Size),
					lastUsed,
				)
			}

			fmt.Println(strings.Repeat("─", 80))
			fmt.Printf("Total: %d entries, %s\n", len(sizes), formatSize(totalSize))

			return nil
		},
	}
}

func buildProjectNameMap(rootPaths []string) map[string]string {
	nameMap := make(map[string]string)
	for _, rootPath := range rootPaths {
		projectID := mono.ComputeProjectID(rootPath)
		nameMap[projectID] = formatProjectName(rootPath)
	}
	return nameMap
}

func formatProjectName(rootPath string) string {
	parts := strings.Split(rootPath, string(os.PathSeparator))
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return rootPath
}

type cacheDisplayEntry struct {
	entry       mono.CacheSizeEntry
	projectName string
	hits        int
	lastUsed    string
	label       string
}

func newCacheCleanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove cached artifacts",
		Long:  "Interactively select and remove cached build artifacts.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := exec.LookPath("fzf"); err != nil {
				return fmt.Errorf("fzf not found (install with: brew install fzf)")
			}

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

			if all {
				count, totalSize, err := cm.RemoveAllCache()
				if err != nil {
					return err
				}
				if err := db.DeleteAllCacheEvents(); err != nil {
					return fmt.Errorf("failed to clear cache events: %w", err)
				}
				fmt.Printf("Removed %d entries (%s)\n", count, formatSize(totalSize))
				return nil
			}

			stats, err := db.GetCacheStats()
			if err != nil {
				return err
			}

			rootPaths, err := db.GetAllRootPaths()
			if err != nil {
				return err
			}

			projectNames := buildProjectNameMap(rootPaths)

			statsMap := make(map[string]mono.CacheEntry)
			for _, s := range stats {
				key := s.ProjectID + "/" + s.Artifact + "/" + s.CacheKey
				statsMap[key] = s
			}

			var displayEntries []cacheDisplayEntry
			for _, entry := range sizes {
				key := entry.ProjectID + "/" + entry.Artifact + "/" + entry.CacheKey

				projectName := entry.ProjectID
				if len(projectName) > 12 {
					projectName = projectName[:12]
				}
				if name, ok := projectNames[entry.ProjectID]; ok {
					projectName = name
				}

				hits := 0
				lastUsed := "never"
				if s, ok := statsMap[key]; ok {
					hits = s.Hits
					lastUsed = formatTimeAgo(s.LastUsed)
				}

				label := fmt.Sprintf("%-20s  %8s   %3d hits   %s",
					projectName+"/"+entry.Artifact,
					formatSize(entry.Size),
					hits,
					lastUsed,
				)

				displayEntries = append(displayEntries, cacheDisplayEntry{
					entry:       entry,
					projectName: projectName,
					hits:        hits,
					lastUsed:    lastUsed,
					label:       label,
				})
			}

			selected, err := selectCachesWithFzf(displayEntries)
			if err != nil {
				return err
			}

			if len(selected) == 0 {
				fmt.Println("No entries selected.")
				return nil
			}

			var totalRemoved int64
			for _, entry := range selected {
				if err := cm.RemoveCacheEntry(entry.ProjectID, entry.Artifact, entry.CacheKey); err != nil {
					return fmt.Errorf("failed to remove %s/%s: %w", entry.ProjectID, entry.Artifact, err)
				}
				if err := db.DeleteCacheEvents(entry.ProjectID, entry.Artifact, entry.CacheKey); err != nil {
					return fmt.Errorf("failed to delete cache events: %w", err)
				}
				totalRemoved += entry.Size
			}

			fmt.Printf("Removed %d entries (%s)\n", len(selected), formatSize(totalRemoved))
			return nil
		},
	}

	cmd.Flags().Bool("all", false, "Remove all cached entries without prompting")

	return cmd
}

func selectCachesWithFzf(entries []cacheDisplayEntry) ([]mono.CacheSizeEntry, error) {
	var lines []string
	for _, e := range entries {
		lines = append(lines, e.label)
	}

	fzf := exec.Command("fzf",
		"--multi",
		"--height=~15",
		"--layout=reverse-list",
		"--no-info",
		"--no-separator",
		"--pointer=>",
		"--prompt=clean> ",
	)
	fzf.Stdin = strings.NewReader(strings.Join(lines, "\n"))
	fzf.Stderr = os.Stderr

	output, err := fzf.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 130 {
			return nil, nil
		}
		return nil, nil
	}

	selectedLines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(selectedLines) == 0 || (len(selectedLines) == 1 && selectedLines[0] == "") {
		return nil, nil
	}

	labelToEntry := make(map[string]mono.CacheSizeEntry)
	for _, e := range entries {
		labelToEntry[e.label] = e.entry
	}

	var selected []mono.CacheSizeEntry
	for _, line := range selectedLines {
		if entry, ok := labelToEntry[line]; ok {
			selected = append(selected, entry)
		}
	}

	return selected, nil
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
