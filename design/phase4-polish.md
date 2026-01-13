# Phase 4: Polish & Statistics

## Overview

This phase adds quality-of-life improvements: cache statistics, time savings reporting, size limits with LRU eviction, and configuration for advanced users.

**Expected outcome**: Users see value of caching through statistics, and cache size stays manageable with automatic cleanup.

## Prerequisites

- Phases 1-3 completed

## Design Decision: Global Statistics in `~/.mono/state.db`

Statistics are stored in the existing `~/.mono/state.db` database, making them global across all projects. This allows users to see aggregate cache performance across their entire workflow.

```
~/.mono/
├── state.db                # includes cache_events table
├── cache_global/           # Layers 1 & 2: shared across ALL projects
│   ├── cargo/
│   ├── npm/
│   └── sccache/
└── cache_local/            # Layer 3: per-project artifact cache
    └── <project-id>/
```

## Files to Modify

| File | Changes |
|------|---------|
| `internal/mono/config.go` | Add cache size limit config |
| `internal/mono/cache.go` | Add statistics tracking, LRU eviction |
| `internal/mono/db.go` | Add cache statistics table |
| `internal/mono/operations.go` | Record timing, report savings |
| `internal/cli/cache.go` | Add `mono cache stats` command |

## Implementation Steps

### Step 1: Add Statistics Schema

**File**: `internal/mono/db.go`

```go
const schemaV2 = `
CREATE TABLE IF NOT EXISTS cache_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type TEXT NOT NULL,      -- 'hit', 'miss', 'store'
    cache_type TEXT NOT NULL,      -- 'cargo', 'npm', etc.
    cache_key TEXT NOT NULL,
    project_id TEXT NOT NULL,
    env_name TEXT NOT NULL,
    duration_ms INTEGER,           -- time saved (hit) or spent (store)
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_cache_events_type ON cache_events(event_type);
CREATE INDEX IF NOT EXISTS idx_cache_events_created ON cache_events(created_at);
CREATE INDEX IF NOT EXISTS idx_cache_events_project ON cache_events(project_id);
`

func (db *DB) RecordCacheEvent(eventType, cacheType, cacheKey, projectID, envName string, durationMs int64) error {
    _, err := db.conn.Exec(`
        INSERT INTO cache_events (event_type, cache_type, cache_key, project_id, env_name, duration_ms)
        VALUES (?, ?, ?, ?, ?, ?)
    `, eventType, cacheType, cacheKey, projectID, envName, durationMs)
    return err
}

type CacheStatsSummary struct {
    TotalHits       int
    TotalMisses     int
    TotalTimeSaved  int64 // milliseconds
    HitsByType      map[string]int
    MissesByType    map[string]int
    TimeSavedByType map[string]int64
}

func (db *DB) GetCacheStats(since time.Time) (CacheStatsSummary, error) {
    stats := CacheStatsSummary{
        HitsByType:      make(map[string]int),
        MissesByType:    make(map[string]int),
        TimeSavedByType: make(map[string]int64),
    }

    rows, err := db.conn.Query(`
        SELECT event_type, cache_type, COUNT(*), COALESCE(SUM(duration_ms), 0)
        FROM cache_events
        WHERE created_at >= ?
        GROUP BY event_type, cache_type
    `, since)
    if err != nil {
        return stats, err
    }
    defer rows.Close()

    for rows.Next() {
        var eventType, cacheType string
        var count int
        var duration int64
        if err := rows.Scan(&eventType, &cacheType, &count, &duration); err != nil {
            return stats, err
        }

        switch eventType {
        case "hit":
            stats.TotalHits += count
            stats.HitsByType[cacheType] = count
            stats.TotalTimeSaved += duration
            stats.TimeSavedByType[cacheType] = duration
        case "miss":
            stats.TotalMisses += count
            stats.MissesByType[cacheType] = count
        }
    }

    return stats, rows.Err()
}

func (db *DB) GetCacheStatsForProject(projectID string, since time.Time) (CacheStatsSummary, error) {
    stats := CacheStatsSummary{
        HitsByType:      make(map[string]int),
        MissesByType:    make(map[string]int),
        TimeSavedByType: make(map[string]int64),
    }

    rows, err := db.conn.Query(`
        SELECT event_type, cache_type, COUNT(*), COALESCE(SUM(duration_ms), 0)
        FROM cache_events
        WHERE project_id = ? AND created_at >= ?
        GROUP BY event_type, cache_type
    `, projectID, since)
    if err != nil {
        return stats, err
    }
    defer rows.Close()

    for rows.Next() {
        var eventType, cacheType string
        var count int
        var duration int64
        if err := rows.Scan(&eventType, &cacheType, &count, &duration); err != nil {
            return stats, err
        }

        switch eventType {
        case "hit":
            stats.TotalHits += count
            stats.HitsByType[cacheType] = count
            stats.TotalTimeSaved += duration
            stats.TimeSavedByType[cacheType] = duration
        case "miss":
            stats.TotalMisses += count
            stats.MissesByType[cacheType] = count
        }
    }

    return stats, rows.Err()
}
```

### Step 2: Add Config for Size Limits

**File**: `internal/mono/config.go`

```go
type CacheConfig struct {
    MaxSize   string `yaml:"max_size"`   // e.g., "10GB", "500MB"
    MaxAge    string `yaml:"max_age"`    // e.g., "30d", "7d"
    AutoClean bool   `yaml:"auto_clean"` // default: true
}

type BuildConfig struct {
    Strategy      string           `yaml:"strategy"`
    DownloadCache bool             `yaml:"download_cache"`
    Sccache       *bool            `yaml:"sccache"`
    Artifacts     []ArtifactConfig `yaml:"artifacts"`
    Cache         CacheConfig      `yaml:"cache"`
}

func (c *CacheConfig) MaxSizeBytes() int64 {
    if c.MaxSize == "" {
        return 0 // unlimited
    }

    size := c.MaxSize
    var multiplier int64 = 1

    switch {
    case strings.HasSuffix(size, "GB"):
        multiplier = 1024 * 1024 * 1024
        size = strings.TrimSuffix(size, "GB")
    case strings.HasSuffix(size, "MB"):
        multiplier = 1024 * 1024
        size = strings.TrimSuffix(size, "MB")
    case strings.HasSuffix(size, "KB"):
        multiplier = 1024
        size = strings.TrimSuffix(size, "KB")
    }

    n, _ := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
    return n * multiplier
}

func (c *CacheConfig) MaxAgeDuration() time.Duration {
    if c.MaxAge == "" {
        return 0 // no limit
    }

    age := c.MaxAge
    var multiplier time.Duration = time.Hour

    switch {
    case strings.HasSuffix(age, "d"):
        multiplier = 24 * time.Hour
        age = strings.TrimSuffix(age, "d")
    case strings.HasSuffix(age, "h"):
        multiplier = time.Hour
        age = strings.TrimSuffix(age, "h")
    }

    n, _ := strconv.ParseInt(strings.TrimSpace(age), 10, 64)
    return time.Duration(n) * multiplier
}
```

### Step 3: Add LRU Eviction

**File**: `internal/mono/cache.go`

```go
func (cm *CacheManager) EnforceQuota(cfg CacheConfig) error {
    maxSize := cfg.MaxSizeBytes()
    maxAge := cfg.MaxAgeDuration()

    if maxSize == 0 && maxAge == 0 {
        return nil
    }

    entries, err := cm.ListCacheEntries()
    if err != nil {
        return err
    }

    sort.Slice(entries, func(i, j int) bool {
        return entries[i].ModTime.Before(entries[j].ModTime)
    })

    var totalSize int64
    for _, e := range entries {
        totalSize += e.Size
    }

    now := time.Now()
    for _, entry := range entries {
        if entry.InUse {
            continue
        }

        shouldEvict := false

        if maxAge > 0 && now.Sub(entry.ModTime) > maxAge {
            shouldEvict = true
        }

        if maxSize > 0 && totalSize > maxSize {
            shouldEvict = true
        }

        if shouldEvict {
            if err := os.RemoveAll(entry.Path); err != nil {
                return err
            }
            totalSize -= entry.Size
        }
    }

    cm.cleanEmptyDirs()

    return nil
}

func (cm *CacheManager) TouchEntry(projectID, cacheType, key string) error {
    entryPath := filepath.Join(cm.LocalCacheDir, projectID, cacheType, key)
    now := time.Now()
    return os.Chtimes(entryPath, now, now)
}
```

### Step 4: Add Timing and Reporting

**File**: `internal/mono/operations.go`

```go
func Init(path string) error {
    initStart := time.Now()

    // ... existing code ...

    projectID := ComputeProjectID(rootPath)

    var timeSaved time.Duration
    var cacheResults []string

    for i := range cacheEntries {
        entry := &cacheEntries[i]
        restoreStart := time.Now()

        if entry.Hit {
            if err := cm.RestoreFromCache(*entry); err != nil {
                entry.Hit = false
            } else {
                restoreDuration := time.Since(restoreStart)
                estimatedBuildTime := estimateBuildTime(*entry)
                saved := estimatedBuildTime - restoreDuration
                if saved > 0 {
                    timeSaved += saved
                }

                cm.TouchEntry(projectID, entry.Name, entry.Key)
                db.RecordCacheEvent("hit", entry.Name, entry.Key, projectID, envName, saved.Milliseconds())
                cacheResults = append(cacheResults, fmt.Sprintf("%s: hit (saved ~%s)", entry.Name, formatDuration(saved)))
            }
        }

        if !entry.Hit {
            db.RecordCacheEvent("miss", entry.Name, entry.Key, projectID, envName, 0)
            cacheResults = append(cacheResults, fmt.Sprintf("%s: miss", entry.Name))
        }
    }

    // ... run build script ...

    for i := range cacheEntries {
        entry := &cacheEntries[i]
        if !entry.Hit {
            storeStart := time.Now()
            if err := cm.StoreToCache(*entry); err == nil {
                storeDuration := time.Since(storeStart)
                db.RecordCacheEvent("store", entry.Name, entry.Key, projectID, envName, storeDuration.Milliseconds())
            }
        }
    }

    if cfg.Build.Cache.AutoClean {
        cm.EnforceQuota(cfg.Build.Cache)
    }

    // Print summary
    fmt.Println()
    fmt.Println("Cache:")
    for _, r := range cacheResults {
        fmt.Printf("  %s\n", r)
    }
    if timeSaved > 0 {
        fmt.Printf("\nSaved ~%s via shared cache\n", formatDuration(timeSaved))
    }

    // ... rest of init ...
}

func estimateBuildTime(entry ArtifactCacheEntry) time.Duration {
    switch entry.Name {
    case "cargo":
        return 5 * time.Minute
    case "npm", "yarn", "pnpm":
        return 2 * time.Minute
    default:
        return 1 * time.Minute
    }
}

func formatDuration(d time.Duration) string {
    if d >= time.Hour {
        return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
    }
    if d >= time.Minute {
        return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
    }
    return fmt.Sprintf("%ds", int(d.Seconds()))
}
```

### Step 5: Add Stats CLI Command

**File**: `internal/cli/cache.go`

```go
func newCacheStatsCmd() *cobra.Command {
    var since string
    var projectFilter string

    cmd := &cobra.Command{
        Use:   "stats",
        Short: "Show cache hit/miss statistics",
        RunE: func(cmd *cobra.Command, args []string) error {
            db, err := mono.OpenDB()
            if err != nil {
                return err
            }
            defer db.Close()

            sinceTime := time.Time{}
            switch since {
            case "today":
                sinceTime = time.Now().Truncate(24 * time.Hour)
            case "week":
                sinceTime = time.Now().AddDate(0, 0, -7)
            case "month":
                sinceTime = time.Now().AddDate(0, -1, 0)
            case "all":
                sinceTime = time.Time{}
            default:
                sinceTime = time.Now().AddDate(0, 0, -7)
            }

            var stats mono.CacheStatsSummary
            if projectFilter != "" {
                stats, err = db.GetCacheStatsForProject(projectFilter, sinceTime)
            } else {
                stats, err = db.GetCacheStats(sinceTime)
            }
            if err != nil {
                return err
            }

            total := stats.TotalHits + stats.TotalMisses
            hitRate := float64(0)
            if total > 0 {
                hitRate = float64(stats.TotalHits) / float64(total) * 100
            }

            header := "Cache Statistics"
            if projectFilter != "" {
                header += fmt.Sprintf(" (project: %s)", projectFilter[:8])
            }
            fmt.Printf("%s (since %s)\n\n", header, formatSince(sinceTime))
            fmt.Printf("Hit Rate:    %.1f%% (%d/%d)\n", hitRate, stats.TotalHits, total)
            fmt.Printf("Time Saved:  %s\n\n", formatDuration(time.Duration(stats.TotalTimeSaved)*time.Millisecond))

            if len(stats.HitsByType) > 0 {
                fmt.Println("By Type:")
                for typ := range stats.HitsByType {
                    hits := stats.HitsByType[typ]
                    misses := stats.MissesByType[typ]
                    total := hits + misses
                    rate := float64(0)
                    if total > 0 {
                        rate = float64(hits) / float64(total) * 100
                    }
                    saved := time.Duration(stats.TimeSavedByType[typ]) * time.Millisecond
                    fmt.Printf("  %-10s %.1f%% hit rate  %s saved\n", typ, rate, formatDuration(saved))
                }
            }

            return nil
        },
    }

    cmd.Flags().StringVar(&since, "since", "week", "Time period: today, week, month, all")
    cmd.Flags().StringVarP(&projectFilter, "project", "p", "", "Filter by project ID")
    return cmd
}

func formatSince(t time.Time) string {
    if t.IsZero() {
        return "all time"
    }
    return t.Format("2006-01-02")
}

func formatDuration(d time.Duration) string {
    if d >= time.Hour {
        return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
    }
    if d >= time.Minute {
        return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
    }
    return fmt.Sprintf("%ds", int(d.Seconds()))
}
```

Update `NewCacheCmd()`:

```go
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
    cmd.AddCommand(newCacheStatsCmd())  // Add this

    return cmd
}
```

## CLI Output Examples

### Init with Cache Hit

```
$ mono init ./env2
Creating environment: myproject/env2

Cache:
  cargo: hit (saved ~4m30s)
  npm: hit (saved ~1m45s)

Saved ~6m15s via shared cache

Starting docker containers...
Running setup script...
Environment ready in 12s
```

### Init with Cache Miss

```
$ mono init ./env1
Creating environment: myproject/env1

Cache:
  cargo: miss
  npm: miss

Building dependencies...
[cargo output]

Storing artifacts to cache...

Running setup script...
Environment ready in 5m23s
```

### Cache Stats (Global)

```
$ mono cache stats
Cache Statistics (since 2024-01-08)

Hit Rate:    78.5% (11/14)
Time Saved:  52m30s

By Type:
  cargo      80.0% hit rate  45m saved
  npm        75.0% hit rate  7m30s saved

$ mono cache stats --since=all
Cache Statistics (since all time)

Hit Rate:    72.3% (34/47)
Time Saved:  3h15m

By Type:
  cargo      75.0% hit rate  2h45m saved
  npm        68.0% hit rate  30m saved
```

### Cache Stats (Per-Project)

```
$ mono cache stats --project=a1b2c3d4
Cache Statistics (project: a1b2c3d4) (since 2024-01-08)

Hit Rate:    85.7% (6/7)
Time Saved:  28m15s

By Type:
  cargo      100.0% hit rate  25m saved
  npm        66.7% hit rate  3m15s saved
```

## Configuration Examples

### Size Limit

```yaml
build:
  cache:
    max_size: 10GB
```

### Age Limit

```yaml
build:
  cache:
    max_age: 30d
```

### Combined

```yaml
build:
  cache:
    max_size: 5GB
    max_age: 14d
    auto_clean: true
```

### Disable Auto-Clean

```yaml
build:
  cache:
    auto_clean: false
```

## Testing

### Manual Test Plan

1. **Statistics tracking**
   ```bash
   mono init ./env1   # miss
   mono init ./env2   # hit
   mono cache stats
   # Should show 50% hit rate, time saved
   ```

2. **Per-project statistics**
   ```bash
   cd ~/project1 && mono init ./env1
   cd ~/project2 && mono init ./env1
   mono cache stats --project=<project1-id>
   # Should only show stats for project1
   ```

3. **Size limit enforcement**
   ```yaml
   build:
     cache:
       max_size: 100MB
   ```
   ```bash
   mono init ./env1   # creates 500MB cache entry
   # Should evict old entries to stay under 100MB
   ```

4. **Age limit enforcement**
   ```yaml
   build:
     cache:
       max_age: 1h
   ```
   ```bash
   # After 2 hours, entry should be evicted
   ```

5. **Touch on hit**
   ```bash
   mono init ./env1
   # Wait
   mono init ./env2  # same lockfile
   ls -l ~/.mono/cache_local/*/cargo/
   # Entry timestamp should be updated
   ```

6. **Stats periods**
   ```bash
   mono cache stats --since=today
   mono cache stats --since=week
   mono cache stats --since=month
   mono cache stats --since=all
   ```

7. **Stats work from any directory**
   ```bash
   cd /tmp
   mono cache stats
   # Should show global stats
   ```

## Acceptance Criteria

- [ ] Cache events recorded in database (hit, miss, store) with project ID
- [ ] `mono cache stats` shows hit rate and time saved globally
- [ ] `mono cache stats --project` filters by project ID
- [ ] `--since` flag works for different time periods
- [ ] Time savings displayed at end of `mono init`
- [ ] `max_size` config enforces disk quota with LRU eviction
- [ ] `max_age` config removes stale entries
- [ ] `auto_clean` runs after each init by default
- [ ] LRU eviction preserves entries still in use
- [ ] Cache entries touched on hit to update LRU order
- [ ] All stats commands work from any directory

## Future Enhancements

- Real build time measurement instead of estimates
- Cache warming command (`mono cache warm`)
- Export stats to JSON for dashboards
- Project name lookup (show path instead of hash)
