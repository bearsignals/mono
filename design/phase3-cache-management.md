# Phase 3: Cache Management CLI

## Overview

This phase implements CLI commands for inspecting and managing the build cache. Users need visibility into cache usage and tools for cleanup.

**Expected outcome**: Users can inspect cache status, clean up old entries, and troubleshoot cache issues.

## Prerequisites

- Phase 1 completed
- Phase 2 completed

## Design Decision: Global Cache Commands

Since the cache lives in `~/.mono/`, cache management commands work globally - they don't require being inside a project directory. This allows users to manage cache from anywhere.

```
~/.mono/
├── state.db
├── cache_global/           # Layers 1 & 2: shared across ALL projects
│   ├── cargo/
│   ├── npm/
│   └── sccache/
└── cache_local/            # Layer 3: per-project artifact cache
    ├── a1b2c3d4e5f6/       # project 1
    │   └── cargo/
    │       └── 7g8h9i0j/
    └── b2c3d4e5f6g7/       # project 2
        └── cargo/
            └── k1l2m3n4/
```

## Files to Create/Modify

| File | Changes |
|------|---------|
| `internal/cli/cache.go` | **New file** - Cache CLI commands |
| `internal/cli/root.go` | Register cache command |
| `internal/mono/cache.go` | Add cache inspection methods |

## Implementation Steps

### Step 1: Add Cache Inspection Methods

**File**: `internal/mono/cache.go`

```go
import (
    "os"
    "path/filepath"
    "syscall"
    "time"
)

type CacheEntry struct {
    ProjectID  string    // project hash
    Type       string    // "cargo", "npm", etc.
    Key        string    // lockfile hash
    Path       string    // full path
    Size       int64     // bytes
    FileCount  int
    ModTime    time.Time
    InUse      bool      // hardlinked by at least one env
}

type CacheStats struct {
    TotalSize       int64
    TotalEntries    int
    EntriesByType   map[string]int
    SizeByType      map[string]int64
    ProjectCount    int
}

func (cm *CacheManager) ListCacheEntries() ([]CacheEntry, error) {
    var entries []CacheEntry

    if !dirExists(cm.LocalCacheDir) {
        return entries, nil
    }

    projects, err := os.ReadDir(cm.LocalCacheDir)
    if err != nil {
        return nil, err
    }

    for _, projectDir := range projects {
        if !projectDir.IsDir() {
            continue
        }

        projectID := projectDir.Name()
        projectPath := filepath.Join(cm.LocalCacheDir, projectID)

        types, err := os.ReadDir(projectPath)
        if err != nil {
            continue
        }

        for _, typeDir := range types {
            if !typeDir.IsDir() {
                continue
            }

            typePath := filepath.Join(projectPath, typeDir.Name())
            keys, err := os.ReadDir(typePath)
            if err != nil {
                continue
            }

            for _, keyDir := range keys {
                if !keyDir.IsDir() {
                    continue
                }

                entryPath := filepath.Join(typePath, keyDir.Name())
                size, fileCount := getDirStats(entryPath)

                info, err := keyDir.Info()
                if err != nil {
                    continue
                }

                entries = append(entries, CacheEntry{
                    ProjectID: projectID,
                    Type:      typeDir.Name(),
                    Key:       keyDir.Name(),
                    Path:      entryPath,
                    Size:      size,
                    FileCount: fileCount,
                    ModTime:   info.ModTime(),
                    InUse:     isEntryInUse(entryPath),
                })
            }
        }
    }

    return entries, nil
}

func (cm *CacheManager) ListCacheEntriesForProject(projectID string) ([]CacheEntry, error) {
    allEntries, err := cm.ListCacheEntries()
    if err != nil {
        return nil, err
    }

    var entries []CacheEntry
    for _, e := range allEntries {
        if e.ProjectID == projectID {
            entries = append(entries, e)
        }
    }
    return entries, nil
}

func getDirStats(path string) (int64, int) {
    var size int64
    var count int

    filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
        if err != nil {
            return nil
        }
        if !info.IsDir() {
            size += info.Size()
            count++
        }
        return nil
    })

    return size, count
}

func isEntryInUse(entryPath string) bool {
    var inUse bool

    filepath.Walk(entryPath, func(path string, info os.FileInfo, err error) error {
        if err != nil || info.IsDir() {
            return nil
        }

        stat, ok := info.Sys().(*syscall.Stat_t)
        if ok && stat.Nlink > 1 {
            inUse = true
            return filepath.SkipAll
        }
        return nil
    })

    return inUse
}

func (cm *CacheManager) GetCacheStats() (CacheStats, error) {
    entries, err := cm.ListCacheEntries()
    if err != nil {
        return CacheStats{}, err
    }

    stats := CacheStats{
        EntriesByType: make(map[string]int),
        SizeByType:    make(map[string]int64),
    }

    projects := make(map[string]bool)
    for _, entry := range entries {
        stats.TotalSize += entry.Size
        stats.TotalEntries++
        stats.EntriesByType[entry.Type]++
        stats.SizeByType[entry.Type] += entry.Size
        projects[entry.ProjectID] = true
    }
    stats.ProjectCount = len(projects)

    return stats, nil
}

func (cm *CacheManager) CleanCache(force bool) (int64, int, error) {
    entries, err := cm.ListCacheEntries()
    if err != nil {
        return 0, 0, err
    }

    var removedSize int64
    var removedCount int

    for _, entry := range entries {
        if !force && entry.InUse {
            continue
        }

        if err := os.RemoveAll(entry.Path); err != nil {
            return removedSize, removedCount, err
        }

        removedSize += entry.Size
        removedCount++
    }

    cm.cleanEmptyDirs()

    return removedSize, removedCount, nil
}

func (cm *CacheManager) cleanEmptyDirs() {
    if !dirExists(cm.LocalCacheDir) {
        return
    }

    projects, _ := os.ReadDir(cm.LocalCacheDir)
    for _, projectDir := range projects {
        if !projectDir.IsDir() {
            continue
        }
        projectPath := filepath.Join(cm.LocalCacheDir, projectDir.Name())

        types, _ := os.ReadDir(projectPath)
        for _, typeDir := range types {
            if !typeDir.IsDir() {
                continue
            }
            typePath := filepath.Join(projectPath, typeDir.Name())

            entries, _ := os.ReadDir(typePath)
            if len(entries) == 0 {
                os.Remove(typePath)
            }
        }

        entries, _ := os.ReadDir(projectPath)
        if len(entries) == 0 {
            os.Remove(projectPath)
        }
    }
}

func (cm *CacheManager) RemoveCacheEntry(projectID, cacheType, key string) error {
    entryPath := filepath.Join(cm.LocalCacheDir, projectID, cacheType, key)
    if !dirExists(entryPath) {
        return fmt.Errorf("cache entry not found: %s/%s/%s", projectID, cacheType, key)
    }
    err := os.RemoveAll(entryPath)
    if err == nil {
        cm.cleanEmptyDirs()
    }
    return err
}

func (cm *CacheManager) VerifyCache() ([]string, error) {
    var errors []string

    entries, err := cm.ListCacheEntries()
    if err != nil {
        return nil, err
    }

    for _, entry := range entries {
        if entry.FileCount == 0 {
            errors = append(errors, fmt.Sprintf("%s/%s/%s: empty cache entry", entry.ProjectID, entry.Type, entry.Key))
        }
    }

    return errors, nil
}
```

### Step 2: Create Cache CLI Commands

**File**: `internal/cli/cache.go` (new)

```go
package cli

import (
    "fmt"
    "os"
    "text/tabwriter"

    "github.com/spf13/cobra"
    "mono/internal/mono"
)

func NewCacheCmd() *cobra.Command {
    cmd := &cobra.Command{
        Use:   "cache",
        Short: "Manage build cache",
    }

    cmd.AddCommand(newCacheStatusCmd())
    cmd.AddCommand(newCacheCleanCmd())
    cmd.AddCommand(newCacheListCmd())
    cmd.AddCommand(newCacheVerifyCmd())
    cmd.AddCommand(newCacheRemoveCmd())

    return cmd
}

func newCacheStatusCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "status",
        Short: "Show cache size and summary",
        RunE: func(cmd *cobra.Command, args []string) error {
            cm, err := mono.NewCacheManager()
            if err != nil {
                return err
            }

            stats, err := cm.GetCacheStats()
            if err != nil {
                return err
            }

            fmt.Printf("Cache Directory: %s\n\n", cm.LocalCacheDir)
            fmt.Printf("Total Size:    %s\n", formatBytes(stats.TotalSize))
            fmt.Printf("Total Entries: %d\n", stats.TotalEntries)
            fmt.Printf("Projects:      %d\n\n", stats.ProjectCount)

            if len(stats.EntriesByType) > 0 {
                fmt.Println("By Type:")
                for typ, count := range stats.EntriesByType {
                    size := stats.SizeByType[typ]
                    fmt.Printf("  %-10s %3d entries  %s\n", typ, count, formatBytes(size))
                }
            }

            return nil
        },
    }
}

func newCacheListCmd() *cobra.Command {
    var projectFilter string

    cmd := &cobra.Command{
        Use:   "list",
        Short: "List all cache entries",
        RunE: func(cmd *cobra.Command, args []string) error {
            cm, err := mono.NewCacheManager()
            if err != nil {
                return err
            }

            var entries []mono.CacheEntry
            if projectFilter != "" {
                entries, err = cm.ListCacheEntriesForProject(projectFilter)
            } else {
                entries, err = cm.ListCacheEntries()
            }
            if err != nil {
                return err
            }

            if len(entries) == 0 {
                fmt.Println("No cache entries found")
                return nil
            }

            w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
            fmt.Fprintln(w, "PROJECT\tTYPE\tKEY\tSIZE\tFILES\tIN USE\tMODIFIED")
            for _, e := range entries {
                inUse := "-"
                if e.InUse {
                    inUse = "yes"
                }
                fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
                    e.ProjectID[:8],
                    e.Type,
                    e.Key,
                    formatBytes(e.Size),
                    e.FileCount,
                    inUse,
                    e.ModTime.Format("2006-01-02 15:04"),
                )
            }
            w.Flush()

            return nil
        },
    }

    cmd.Flags().StringVarP(&projectFilter, "project", "p", "", "Filter by project ID")
    return cmd
}

func newCacheCleanCmd() *cobra.Command {
    var force bool

    cmd := &cobra.Command{
        Use:   "clean",
        Short: "Remove cache entries",
        Long:  "Remove unused cache entries. Use --force to remove all entries including those in use.",
        RunE: func(cmd *cobra.Command, args []string) error {
            cm, err := mono.NewCacheManager()
            if err != nil {
                return err
            }

            removedSize, removedCount, err := cm.CleanCache(force)
            if err != nil {
                return err
            }

            if removedCount == 0 {
                if force {
                    fmt.Println("No cache entries to remove")
                } else {
                    fmt.Println("No unused cache entries to remove")
                    fmt.Println("Use --force to remove entries still in use")
                }
                return nil
            }

            fmt.Printf("Removed %d entries (%s)\n", removedCount, formatBytes(removedSize))
            return nil
        },
    }

    cmd.Flags().BoolVarP(&force, "force", "f", false, "Remove all entries including those in use")
    return cmd
}

func newCacheVerifyCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "verify",
        Short: "Check cache entries for corruption",
        RunE: func(cmd *cobra.Command, args []string) error {
            cm, err := mono.NewCacheManager()
            if err != nil {
                return err
            }

            errors, err := cm.VerifyCache()
            if err != nil {
                return err
            }

            if len(errors) == 0 {
                fmt.Println("All cache entries verified successfully")
                return nil
            }

            fmt.Println("Cache verification found issues:")
            for _, e := range errors {
                fmt.Printf("  - %s\n", e)
            }
            return nil
        },
    }
}

func newCacheRemoveCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "remove <project-id> <type> <key>",
        Short: "Remove a specific cache entry",
        Args:  cobra.ExactArgs(3),
        RunE: func(cmd *cobra.Command, args []string) error {
            cm, err := mono.NewCacheManager()
            if err != nil {
                return err
            }

            projectID := args[0]
            cacheType := args[1]
            key := args[2]

            if err := cm.RemoveCacheEntry(projectID, cacheType, key); err != nil {
                return err
            }

            fmt.Printf("Removed cache entry: %s/%s/%s\n", projectID, cacheType, key)
            return nil
        },
    }
}

func formatBytes(bytes int64) string {
    const (
        KB = 1024
        MB = KB * 1024
        GB = MB * 1024
    )

    switch {
    case bytes >= GB:
        return fmt.Sprintf("%.1f GB", float64(bytes)/GB)
    case bytes >= MB:
        return fmt.Sprintf("%.1f MB", float64(bytes)/MB)
    case bytes >= KB:
        return fmt.Sprintf("%.1f KB", float64(bytes)/KB)
    default:
        return fmt.Sprintf("%d B", bytes)
    }
}
```

### Step 3: Register Cache Command

**File**: `internal/cli/root.go`

```go
func NewRootCmd() *cobra.Command {
    rootCmd := &cobra.Command{
        Use:   "mono",
        Short: "Mono development environment manager",
    }

    rootCmd.AddCommand(newInitCmd())
    rootCmd.AddCommand(newDestroyCmd())
    rootCmd.AddCommand(newRunCmd())
    rootCmd.AddCommand(newListCmd())
    rootCmd.AddCommand(NewCacheCmd())  // Add this line

    return rootCmd
}
```

## CLI Usage Examples

### `mono cache status`

```
$ mono cache status
Cache Directory: /Users/x/.mono/cache_local

Total Size:    2.4 GB
Total Entries: 5
Projects:      2

By Type:
  cargo        3 entries  2.1 GB
  npm          2 entries  312.5 MB
```

### `mono cache list`

```
$ mono cache list
PROJECT   TYPE    KEY         SIZE      FILES  IN USE  MODIFIED
a1b2c3d4  cargo   7g8h9i0j    1.2 GB    8432   yes     2024-01-15 10:30
a1b2c3d4  cargo   e5f6g7h8    856.2 MB  7891   -       2024-01-14 15:22
a1b2c3d4  npm     m3n4o5p6    212.3 MB  15432  yes     2024-01-15 10:32
b2c3d4e5  cargo   i9j0k1l2    95.4 MB   2341   -       2024-01-10 09:15
b2c3d4e5  npm     q7r8s9t0    100.2 MB  12100  -       2024-01-12 14:45

$ mono cache list --project=a1b2c3d4
PROJECT   TYPE    KEY         SIZE      FILES  IN USE  MODIFIED
a1b2c3d4  cargo   7g8h9i0j    1.2 GB    8432   yes     2024-01-15 10:30
a1b2c3d4  cargo   e5f6g7h8    856.2 MB  7891   -       2024-01-14 15:22
a1b2c3d4  npm     m3n4o5p6    212.3 MB  15432  yes     2024-01-15 10:32
```

### `mono cache clean`

```
$ mono cache clean
Removed 3 entries (1.0 GB)

$ mono cache clean --force
Removed 5 entries (2.4 GB)
```

### `mono cache verify`

```
$ mono cache verify
All cache entries verified successfully

# or with issues:
$ mono cache verify
Cache verification found issues:
  - a1b2c3d4/cargo/7g8h9i0j: empty cache entry
```

### `mono cache remove`

```
$ mono cache remove a1b2c3d4 cargo 7g8h9i0j
Removed cache entry: a1b2c3d4/cargo/7g8h9i0j
```

## Testing

### Manual Test Plan

1. **Status with empty cache**
   ```bash
   rm -rf ~/.mono/cache_local
   mono cache status
   # Should show "Total Size: 0 B, Total Entries: 0"
   ```

2. **Status after builds**
   ```bash
   cd ~/project1 && mono init ./env1
   cd ~/project2 && mono init ./env1
   mono cache status
   # Should show entries from both projects
   ```

3. **List entries**
   ```bash
   mono cache list
   # Should show table with all entries across all projects
   # "IN USE" should be "yes" for entries with active hardlinks
   ```

4. **List entries for specific project**
   ```bash
   mono cache list --project=a1b2c3d4
   # Should only show entries for that project
   ```

5. **Clean unused**
   ```bash
   mono destroy ~/project1/env1
   mono cache clean
   # Should remove entries no longer hardlinked
   ```

6. **Clean force**
   ```bash
   mono cache clean --force
   # Should remove all entries
   ```

7. **Remove specific entry**
   ```bash
   mono cache remove a1b2c3d4 cargo 7g8h9i0j
   # Should remove only that entry
   ```

8. **Verify cache**
   ```bash
   mono cache verify
   # Should detect any empty or corrupted entries
   ```

9. **Commands work from any directory**
   ```bash
   cd /tmp
   mono cache status
   # Should work without being in a project
   ```

## Acceptance Criteria

- [ ] `mono cache status` shows summary with total size, entries by type, and project count
- [ ] `mono cache list` shows detailed table with project ID column
- [ ] `mono cache list --project` filters by project ID
- [ ] `mono cache clean` removes unused entries only
- [ ] `mono cache clean --force` removes all entries
- [ ] `mono cache verify` detects empty/corrupted entries
- [ ] `mono cache remove <project> <type> <key>` removes specific entry
- [ ] All commands work from any directory (not project-specific)
- [ ] Empty project/type directories cleaned up after removal
- [ ] Human-readable size formatting (KB, MB, GB)

## Future Enhancements (Phase 4)

- Cache age display
- LRU-based automatic cleanup
- Size limits with automatic eviction
- Cache hit/miss statistics
