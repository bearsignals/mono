# Cache Seeding

## Overview

Seeding populates the cache from existing artifacts in the project root (main branch) before the first build. This avoids redundant builds when the user's main branch already has `target/` or `node_modules/` built.

## Problem

When creating a new worktree via `mono init`:

1. Git worktrees don't include `target/` or `node_modules/` (they're in `.gitignore`)
2. Cache is empty on first use
3. Build runs from scratch, even though main branch has identical dependencies already built

```
/project/
├── main/                    # main branch
│   ├── Cargo.lock           # dependencies A, B, C
│   └── target/              # already built!
└── environments/
    └── feature-1/           # new worktree
        ├── Cargo.lock       # same dependencies A, B, C
        └── (no target/)     # cache miss, rebuilds everything
```

## Solution

Before running the build, check if project root has artifacts with matching lockfile. If so, seed the cache by hardlinking from root.

```
After seeding:
~/.mono/cache_local/<project>/cargo/<hash>/target/  → hardlinked from main/target/

After restore:
/project/environments/feature-1/target/  → hardlinked from cache
```

## When Seeding Runs

During `mono init`, after cache miss detection, before running build script.

```
mono init flow:
1. Compute cache key for new environment
2. Check cache → hit? restore and skip build
3. Cache miss → attempt seed from root
4. Check cache again → hit? restore and skip build
5. Still miss → run build, store to cache
```

## Implementation

### Seed Function

**File**: `internal/mono/cache.go`

```go
func (cm *CacheManager) SeedFromRoot(artifacts []ArtifactConfig, rootPath, envPath string) error {
    for _, artifact := range artifacts {
        if err := cm.seedArtifactFromRoot(artifact, rootPath, envPath); err != nil {
            return err
        }
    }
    return nil
}

func (cm *CacheManager) seedArtifactFromRoot(artifact ArtifactConfig, rootPath, envPath string) error {
    if rootPath == envPath {
        return nil // environment IS the root, nothing to seed from
    }

    envKey, err := cm.ComputeCacheKey(artifact, envPath)
    if err != nil {
        return nil // lockfile missing in env, skip
    }

    cachePath := cm.GetArtifactCachePath(rootPath, artifact.Name, envKey)
    if dirExists(cachePath) {
        return nil // already cached
    }

    rootKey, err := cm.ComputeCacheKey(artifact, rootPath)
    if err != nil {
        return nil // lockfile missing in root, skip
    }

    if envKey != rootKey {
        return nil // lockfiles differ, can't seed
    }

    for _, p := range artifact.Paths {
        rootArtifact := filepath.Join(rootPath, p)
        if !dirExists(rootArtifact) {
            continue
        }

        if err := cm.seedToCache(rootArtifact, cachePath); err != nil {
            return fmt.Errorf("failed to seed %s from root: %w", artifact.Name, err)
        }
    }

    return nil
}

func (cm *CacheManager) seedToCache(sourcePath, cachePath string) error {
    if err := os.MkdirAll(cachePath, 0755); err != nil {
        return err
    }

    targetInCache := filepath.Join(cachePath, filepath.Base(sourcePath))

    if dirExists(targetInCache) {
        return nil // another process seeded it
    }

    return HardlinkTree(sourcePath, targetInCache)
}
```

### Integration in Init

**File**: `internal/mono/operations.go`

```go
func Init(path string) error {
    // ... existing setup ...

    cacheEntries, err := cm.PrepareArtifactCache(cfg.Build.Artifacts, rootPath, envPath)
    if err != nil {
        logger.Warn("failed to prepare artifact cache: %v", err)
    }

    // Check for cache misses
    hasMiss := false
    for _, entry := range cacheEntries {
        if !entry.Hit {
            hasMiss = true
            break
        }
    }

    // Attempt to seed from root if we have misses
    if hasMiss {
        if err := cm.SeedFromRoot(cfg.Build.Artifacts, rootPath, envPath); err != nil {
            logger.Warn("failed to seed cache from root: %v", err)
        }

        // Re-check cache after seeding
        cacheEntries, err = cm.PrepareArtifactCache(cfg.Build.Artifacts, rootPath, envPath)
        if err != nil {
            logger.Warn("failed to prepare artifact cache: %v", err)
        }
    }

    // ... continue with restore/build flow ...
}
```

## Why Hardlink (Not Move)

The project root (main branch) may still be in active use. We cannot move its artifacts.

| Approach | Root's artifacts | Cache | Safe? |
|----------|------------------|-------|-------|
| Move | Gone | Has copy | No - breaks main |
| Copy | Intact | Has copy | Yes but slow, uses 2x disk |
| Hardlink | Intact | Shares inodes | Yes, fast, no extra disk |

Hardlinking is ideal:
- Root keeps working
- Cache populated instantly
- No additional disk usage
- New environment hardlinks from cache (standard flow)

## Edge Cases

### 1. Environment is the root

**Scenario**: User runs `mono init .` in project root itself.

**Mitigation**: Skip seeding if `rootPath == envPath`.

```go
if rootPath == envPath {
    return nil
}
```

### 2. Lockfiles differ

**Scenario**: Main has different dependencies than new environment (different branch).

**Mitigation**: Compare cache keys, skip if different.

```go
if envKey != rootKey {
    return nil
}
```

### 3. Root has no artifacts

**Scenario**: Main branch never built, no `target/` exists.

**Mitigation**: Check existence before attempting seed.

```go
if !dirExists(rootArtifact) {
    continue
}
```

### 4. Root's artifacts are stale

**Scenario**: Root has `target/` but lockfile changed since last build.

**Mitigation**: Cache key is computed from lockfile. If root's lockfile changed but `target/` is old, the keys won't match and seeding is skipped. User would need to rebuild main first.

### 5. Partial artifacts in root

**Scenario**: Root has `target/` but build was interrupted, artifacts incomplete.

**Mitigation**: Not directly handled. The incomplete artifacts get cached, and subsequent build in the new environment will complete them (incremental build). This is acceptable.

### 6. Cross-filesystem

**Scenario**: Root and cache on different filesystems, hardlinks fail.

**Mitigation**: Fall back to copy (handled in HardlinkTree).

```go
func HardlinkTree(src, dst string) error {
    // ... existing code ...
    if err := os.Link(path, dstPath); err != nil {
        if isHardlinkNotSupported(err) {
            return copyFile(path, dstPath)
        }
        return err
    }
    // ...
}
```

### 7. Build in progress in root

**Scenario**: Main branch is actively building when we try to seed.

**Mitigation**: Check for build lock file.

```go
func (cm *CacheManager) seedArtifactFromRoot(artifact ArtifactConfig, rootPath, envPath string) error {
    // ... existing checks ...

    if cm.isBuildInProgress(rootPath, artifact) {
        return nil // root is building, skip seeding
    }

    // ... proceed with seed ...
}
```

### 8. Concurrent seeding

**Scenario**: Two `mono init` calls try to seed the same cache entry simultaneously.

**Mitigation**: Check if cache entry exists after mkdir. First one wins.

```go
func (cm *CacheManager) seedToCache(sourcePath, cachePath string) error {
    if err := os.MkdirAll(cachePath, 0755); err != nil {
        return err
    }

    targetInCache := filepath.Join(cachePath, filepath.Base(sourcePath))

    if dirExists(targetInCache) {
        return nil // another process seeded it
    }

    return HardlinkTree(sourcePath, targetInCache)
}
```

## Testing

### Manual Test Plan

1. **Basic seeding**
   ```bash
   cd ~/project/main && cargo build   # build main first
   mono init ./envs/feature-1
   # Should see: "seeded cargo cache from root"
   # Should NOT run cargo build
   ls -i main/target/debug/deps/libserde*.rlib envs/feature-1/target/debug/deps/libserde*.rlib
   # Should show same inode (hardlinked)
   ```

2. **Different lockfiles (no seed)**
   ```bash
   cd ~/project/main && cargo build
   cd ~/project/envs/feature-1
   git checkout feature-branch        # has different Cargo.lock
   mono init .
   # Should NOT seed (lockfiles differ)
   # Should run cargo build
   ```

3. **Root has no artifacts**
   ```bash
   rm -rf ~/project/main/target
   mono init ./envs/feature-1
   # Should skip seeding
   # Should run cargo build
   ```

4. **Seeding with npm**
   ```bash
   cd ~/project/main && npm install
   mono init ./envs/feature-1
   # Should seed node_modules from main
   ```

5. **Environment is root**
   ```bash
   cd ~/project/main
   mono init .
   # Should NOT attempt to seed from itself
   ```

## Logging

```
# Successful seed
[mono] cache miss for cargo (key: abc123)
[mono] seeded cargo cache from project root
[mono] cache hit for cargo (key: abc123)

# No seed (different lockfiles)
[mono] cache miss for cargo (key: abc123)
[mono] root has different lockfile, cannot seed
[mono] running build script...

# No seed (root has no artifacts)
[mono] cache miss for cargo (key: abc123)
[mono] root has no cargo artifacts to seed
[mono] running build script...
```

## Summary

| Scenario | Root has artifacts? | Keys match? | Action |
|----------|---------------------|-------------|--------|
| Fresh project | No | N/A | Normal build |
| Main built, same deps | Yes | Yes | Seed → restore |
| Main built, different deps | Yes | No | Normal build |
| Main building | In progress | N/A | Skip seed, normal build |
