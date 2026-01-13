# Cache Sync

## Overview

Sync ensures the current state of build artifacts (`target/`, `node_modules/`) is stored in the cache. This captures work done after initial environment creation, such as adding new dependencies.

## When Sync Runs

1. **On destroy**: Automatically before environment deletion (captures final state)
2. **Manual**: User runs `mono sync <path>` to explicitly sync an environment

## How It Works

For each artifact type (cargo, npm, etc.):

1. Compute cache key from current lockfile
2. If cache entry already exists → skip (nothing to do)
3. If cache entry missing → move artifacts to cache
4. If environment will continue to be used → hardlink back

```
Before sync:
  env/target/  (local, potentially modified)
  cache_local/<project>/cargo/<old-key>/target/  (old cache entry)

After sync:
  env/target/  → hardlinks to cache (if HardlinkBack=true)
  cache_local/<project>/cargo/<new-key>/target/  (new cache entry)
```

## Implementation

### SyncOptions

```go
type SyncOptions struct {
    HardlinkBack bool // false when called from destroy
}
```

### Sync Function

**File**: `internal/mono/cache.go`

```go
func (cm *CacheManager) Sync(artifacts []ArtifactConfig, rootPath, envPath string, opts SyncOptions) error {
    for _, artifact := range artifacts {
        if err := cm.syncArtifact(artifact, rootPath, envPath, opts); err != nil {
            return err
        }
    }
    return nil
}

func (cm *CacheManager) syncArtifact(artifact ArtifactConfig, rootPath, envPath string, opts SyncOptions) error {
    key, err := cm.ComputeCacheKey(artifact, envPath)
    if err != nil {
        return nil // lockfile missing, skip silently
    }

    cachePath := cm.GetArtifactCachePath(rootPath, artifact.Name, key)

    if dirExists(cachePath) {
        return nil // already cached
    }

    for _, p := range artifact.Paths {
        localPath := filepath.Join(envPath, p)

        if !dirExists(localPath) {
            continue
        }

        if err := cm.moveToCache(localPath, cachePath, opts.HardlinkBack); err != nil {
            return fmt.Errorf("failed to sync %s: %w", artifact.Name, err)
        }
    }

    return nil
}

func (cm *CacheManager) moveToCache(localPath, cachePath string, hardlinkBack bool) error {
    if err := os.MkdirAll(cachePath, 0755); err != nil {
        return err
    }

    targetInCache := filepath.Join(cachePath, filepath.Base(localPath))

    if err := os.Rename(localPath, targetInCache); err != nil {
        if isCrossDevice(err) {
            return cm.copyToCache(localPath, targetInCache, hardlinkBack)
        }
        return err
    }

    if hardlinkBack {
        return HardlinkTree(targetInCache, localPath)
    }

    return nil
}

func (cm *CacheManager) copyToCache(localPath, targetInCache string, hardlinkBack bool) error {
    if err := copyDir(localPath, targetInCache); err != nil {
        return err
    }

    if hardlinkBack {
        return nil // keep local copy as-is, no hardlinks possible cross-device
    }

    return os.RemoveAll(localPath)
}

func isCrossDevice(err error) bool {
    return strings.Contains(err.Error(), "cross-device link") ||
        strings.Contains(err.Error(), "invalid cross-device link")
}
```

## Edge Cases

### 1. Build in progress

**Risk**: Syncing while `cargo build` is running corrupts the cache.

**Mitigation**: Check for Cargo's lock file before syncing.

```go
func (cm *CacheManager) isBuildInProgress(envPath string, artifact ArtifactConfig) bool {
    switch artifact.Name {
    case "cargo":
        lockFile := filepath.Join(envPath, "target", ".cargo-lock")
        return fileExists(lockFile)
    default:
        return false
    }
}

func (cm *CacheManager) syncArtifact(artifact ArtifactConfig, rootPath, envPath string, opts SyncOptions) error {
    if cm.isBuildInProgress(envPath, artifact) {
        return fmt.Errorf("build in progress, cannot sync %s", artifact.Name)
    }
    // ... rest of sync
}
```

### 2. Lockfile missing

**Risk**: Cannot compute cache key.

**Mitigation**: Skip that artifact type silently. This is normal for projects that don't use that package manager.

```go
key, err := cm.ComputeCacheKey(artifact, envPath)
if err != nil {
    return nil // skip silently
}
```

### 3. Disk full

**Risk**: Partial move leaves environment in broken state.

**Mitigation**: Check available disk space before moving. If move fails, attempt recovery.

```go
func (cm *CacheManager) moveToCache(localPath, cachePath string, hardlinkBack bool) error {
    if err := os.MkdirAll(cachePath, 0755); err != nil {
        return err
    }

    targetInCache := filepath.Join(cachePath, filepath.Base(localPath))

    if err := os.Rename(localPath, targetInCache); err != nil {
        return err
    }

    if hardlinkBack {
        if err := HardlinkTree(targetInCache, localPath); err != nil {
            // Recovery: move back to original location
            os.Rename(targetInCache, localPath)
            os.Remove(cachePath) // clean up empty cache dir
            return fmt.Errorf("failed to hardlink back, recovered: %w", err)
        }
    }

    return nil
}
```

### 4. Cross-filesystem

**Risk**: `os.Rename` fails when cache and environment are on different filesystems.

**Mitigation**: Detect cross-device error, fall back to copy.

```go
if err := os.Rename(localPath, targetInCache); err != nil {
    if isCrossDevice(err) {
        return cm.copyToCache(localPath, targetInCache, hardlinkBack)
    }
    return err
}
```

### 5. Artifacts don't exist

**Risk**: Trying to sync non-existent `target/` directory.

**Mitigation**: Check existence before operating.

```go
if !dirExists(localPath) {
    continue
}
```

### 6. Concurrent syncs

**Risk**: Two environments with same lockfile syncing simultaneously, both try to create same cache entry.

**Mitigation**: Use atomic directory creation. First one wins, second detects and skips.

```go
func (cm *CacheManager) moveToCache(localPath, cachePath string, hardlinkBack bool) error {
    targetInCache := filepath.Join(cachePath, filepath.Base(localPath))

    // Atomic check-and-create using mkdir
    if err := os.MkdirAll(cachePath, 0755); err != nil {
        return err
    }

    // Check again after mkdir (another process might have created it)
    if dirExists(targetInCache) {
        // Another sync won the race, we can skip
        // Optionally: remove local and hardlink from existing cache
        return nil
    }

    // ... proceed with rename
}
```

### 7. Permissions issues

**Risk**: Cannot read artifacts or write to cache directory.

**Mitigation**: Return clear error, let caller handle.

```go
if err := os.Rename(localPath, targetInCache); err != nil {
    return fmt.Errorf("failed to move %s to cache (check permissions): %w", localPath, err)
}
```

## Integration Points

### In Destroy

**File**: `internal/mono/operations.go`

```go
func Destroy(path string) error {
    // ... existing setup ...

    cfg, err := LoadConfig(envPath)
    if err == nil {
        cfg.ApplyDefaults(envPath)

        cm, err := NewCacheManager()
        if err == nil {
            // Sync before destroy - don't hardlink back
            cm.Sync(cfg.Build.Artifacts, rootPath, envPath, SyncOptions{
                HardlinkBack: false,
            })
        }
    }

    // ... continue with destroy (kill tmux, stop containers, etc.) ...
}
```

### CLI Command

**File**: `internal/cli/sync.go`

```go
func newSyncCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "sync <path>",
        Short: "Sync build artifacts to cache",
        Args:  cobra.ExactArgs(1),
        RunE: func(cmd *cobra.Command, args []string) error {
            envPath, err := filepath.Abs(args[0])
            if err != nil {
                return err
            }

            env, err := mono.GetEnvironmentByPath(envPath)
            if err != nil {
                return fmt.Errorf("environment not found: %w", err)
            }

            cfg, err := mono.LoadConfig(envPath)
            if err != nil {
                return err
            }
            cfg.ApplyDefaults(envPath)

            cm, err := mono.NewCacheManager()
            if err != nil {
                return err
            }

            err = cm.Sync(cfg.Build.Artifacts, env.RootPath, envPath, mono.SyncOptions{
                HardlinkBack: true,
            })
            if err != nil {
                return err
            }

            fmt.Println("Sync complete")
            return nil
        },
    }
}
```

## Testing

### Manual Test Plan

1. **Basic sync**
   ```bash
   mono init ./env1
   cd env1 && cargo add serde && cargo build
   mono sync ./env1
   ls ~/.mono/cache_local/*/cargo/
   # Should show new cache entry
   ```

2. **Sync already cached**
   ```bash
   mono sync ./env1
   # Should complete instantly (already cached)
   ```

3. **Sync on destroy**
   ```bash
   mono init ./env1
   cd env1 && cargo add tokio && cargo build
   mono destroy ./env1
   ls ~/.mono/cache_local/*/cargo/
   # Should show cache entry even though env is destroyed
   ```

4. **Build in progress**
   ```bash
   cd env1 && cargo build &
   mono sync ./env1
   # Should error: "build in progress"
   ```

5. **Environment continues working after sync**
   ```bash
   mono sync ./env1
   cd env1 && cargo build
   # Should work, not rebuild everything
   ```

## Summary

| Scenario | HardlinkBack | Behavior |
|----------|--------------|----------|
| `mono sync <path>` | true | Cache artifacts, env keeps working |
| `mono destroy <path>` | false | Cache artifacts, env deleted after |
