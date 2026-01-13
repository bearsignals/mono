# Hardlink Cache Design Flaws Analysis

## Overview

This document analyzes potential edge cases and flaws in the hardlink-based caching system described in the phase documents, sync.md, and seed.md.

## Issue 1: Incremental Compilation Fingerprints

**Severity**: High

**Problem**: Cargo stores absolute paths in `.fingerprint` files inside `target/`. When artifacts are hardlinked to a different path, Cargo detects the path mismatch and triggers a full rebuild, defeating the purpose of caching.

```
# Example fingerprint file content
/Users/gwuah/project/main/src/lib.rs
1704067200  # mtime
abc123      # hash

# After restore to different path:
/Users/gwuah/project/envs/feature-1/src/lib.rs  # Different path!
# Cargo sees path mismatch → full rebuild
```

**Impact**: Cache hits may still result in full rebuilds for Rust projects.

**Solution Options**:

1. **Clean fingerprints after restore** (Recommended)
   - Delete `.fingerprint` directories after hardlinking
   - Cargo rebuilds fingerprints quickly (metadata only)
   - Compiled artifacts (*.rlib, *.rmeta) are preserved

2. **Use relative paths**
   - Set CARGO_TARGET_DIR to relative path
   - Doesn't solve the source file path issue

3. **Patch fingerprints**
   - Rewrite absolute paths in fingerprint files
   - Complex, brittle, version-dependent

**Recommended Fix**:

```go
func (cm *CacheManager) CleanFingerprints(targetDir string) error {
    fingerprintDir := filepath.Join(targetDir, ".fingerprint")
    if dirExists(fingerprintDir) {
        return os.RemoveAll(fingerprintDir)
    }
    return nil
}

func (cm *CacheManager) RestoreArtifact(artifact ArtifactConfig, cachePath, envPath string) error {
    for _, p := range artifact.Paths {
        dst := filepath.Join(envPath, p)
        src := filepath.Join(cachePath, filepath.Base(p))

        if err := HardlinkTree(src, dst); err != nil {
            return err
        }

        if artifact.Name == "cargo" {
            cm.CleanFingerprints(dst)
        }
    }
    return nil
}
```

**Trade-off**: First build after restore takes ~5-10 seconds longer to regenerate fingerprints, but this is far better than a full rebuild (minutes to hours).

---

## Issue 2: Absolute Paths in node_modules

**Severity**: Medium

**Problem**: npm stores absolute paths in several places:

1. `.bin/` symlinks point to absolute paths
2. `package.json` `_where` field contains install path
3. Some packages store absolute paths in postinstall scripts

```
# Example .bin/eslint symlink
/Users/gwuah/project/main/node_modules/.bin/eslint
  → /Users/gwuah/project/main/node_modules/eslint/bin/eslint.js

# After restore to different path, symlink is broken
```

**Impact**: CLI tools in `node_modules/.bin/` may not work after restore.

**Solution Options**:

1. **Regenerate .bin/ after restore** (Recommended)
   - Delete `.bin/` directory
   - Run `npm rebuild` or just recreate symlinks

2. **Use pnpm**
   - pnpm uses a content-addressable store with symlinks
   - Less sensitive to absolute paths

3. **Re-run npm install**
   - Fast with populated cache
   - Ensures all paths are correct

**Recommended Fix**:

```go
func (cm *CacheManager) FixNodeModulesBin(nodeModulesDir string) error {
    binDir := filepath.Join(nodeModulesDir, ".bin")
    if !dirExists(binDir) {
        return nil
    }

    if err := os.RemoveAll(binDir); err != nil {
        return err
    }

    return nil
}
```

After restore, the build script should include `npm rebuild` or the mono init flow should run it automatically. This regenerates `.bin/` symlinks with correct paths.

---

## Issue 3: Cache Corruption Propagation

**Severity**: Medium

**Problem**: Hardlinks share the same inode. If one environment corrupts a cached file (disk error, interrupted write, buggy build tool), all environments sharing that cache entry see the corruption.

```
# Scenario:
env1/target/libfoo.rlib  ─┐
env2/target/libfoo.rlib  ─┼─→ inode 12345 (same physical data)
env3/target/libfoo.rlib  ─┘

# If env1's build corrupts the file:
# All three environments now have corrupted libfoo.rlib
```

**Impact**: Single corruption can break multiple environments.

**Solution Options**:

1. **Checksum verification** (Recommended)
   - Store checksums when caching
   - Verify before restore
   - Rebuild on mismatch

2. **Copy-on-write filesystem**
   - Use btrfs/zfs snapshots instead of hardlinks
   - Platform-dependent

3. **Read-only cache**
   - Mark cache as read-only after storing
   - Builds would fail instead of corrupting
   - Too restrictive for incremental builds

**Recommended Fix**:

```go
type CacheMetadata struct {
    Key       string            `json:"key"`
    CreatedAt time.Time         `json:"created_at"`
    Checksums map[string]string `json:"checksums"`
}

func (cm *CacheManager) ComputeChecksums(dir string) (map[string]string, error) {
    checksums := make(map[string]string)

    err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
        if err != nil || info.IsDir() {
            return err
        }

        if strings.HasSuffix(path, ".rlib") || strings.HasSuffix(path, ".rmeta") {
            rel, _ := filepath.Rel(dir, path)
            hash, err := hashFile(path)
            if err != nil {
                return err
            }
            checksums[rel] = hash
        }
        return nil
    })

    return checksums, err
}

func (cm *CacheManager) VerifyCache(cachePath string, metadata CacheMetadata) error {
    for relPath, expectedHash := range metadata.Checksums {
        fullPath := filepath.Join(cachePath, relPath)
        actualHash, err := hashFile(fullPath)
        if err != nil {
            return fmt.Errorf("cannot read %s: %w", relPath, err)
        }
        if actualHash != expectedHash {
            return fmt.Errorf("checksum mismatch for %s", relPath)
        }
    }
    return nil
}
```

**Trade-off**: Adds overhead to cache and restore operations. Consider sampling (verify random subset) for large caches.

---

## Issue 4: Stale Cache After Root Modification

**Severity**: Low

**Problem**: When seeding from root, if root's `target/` was built with an older Cargo.lock but the lockfile has since been updated, the cache key matches the new lockfile but contains stale artifacts.

```
# Timeline:
1. main/Cargo.lock v1, main/target/ built for v1
2. User updates main/Cargo.lock to v2 (but doesn't rebuild)
3. mono init creates env with Cargo.lock v2
4. Seeding computes key from v2, but root's target/ is for v1
5. Cache entry created with mismatched artifacts
```

**Impact**: Rare scenario, but can cause confusing build failures.

**Solution Options**:

1. **Don't seed if root target is stale** (Complex)
   - Would need to track when target/ was last built
   - No reliable way to know this

2. **Let fingerprint cleaning handle it** (Recommended)
   - Clean fingerprints after restore (Issue #1 fix)
   - Cargo detects stale artifacts and rebuilds as needed
   - Self-correcting behavior

**Recommended Fix**: No additional fix needed. The fingerprint cleaning from Issue #1 handles this case. Cargo's incremental compilation correctly detects which artifacts are stale and rebuilds only those.

---

## Issue 5: Concurrent Build + Sync Race

**Severity**: Medium

**Problem**: The sync operation checks if cache exists, then moves artifacts. This is not atomic. Another process could create the same cache entry between check and move.

```
# Race condition:
Process A: syncArtifact()
  1. Check cache exists → false
  2. --- context switch ---
Process B: syncArtifact() (same key)
  1. Check cache exists → false
  2. Move artifacts to cache → success
  3. --- context switch ---
Process A:
  3. Move artifacts to cache → ERROR (path exists)
```

**Impact**: Sync operation fails with confusing error.

**Solution Options**:

1. **File locking** (Recommended)
   - Use lock file per cache key
   - First process wins, others skip

2. **Atomic rename with temp directory**
   - Move to temp first, then atomic rename to final
   - Rename fails if target exists (first one wins)

3. **Check after move**
   - If move fails because target exists, delete source and continue
   - Another process already cached it

**Recommended Fix**:

```go
func (cm *CacheManager) acquireCacheLock(cachePath string) (*os.File, error) {
    lockPath := cachePath + ".lock"

    if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
        return nil, err
    }

    f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0644)
    if err != nil {
        return nil, err
    }

    if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
        f.Close()
        return nil, fmt.Errorf("cache locked by another process")
    }

    return f, nil
}

func (cm *CacheManager) releaseCacheLock(f *os.File) {
    if f != nil {
        syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
        f.Close()
    }
}

func (cm *CacheManager) moveToCache(localPath, cachePath string, hardlinkBack bool) error {
    lock, err := cm.acquireCacheLock(cachePath)
    if err != nil {
        return nil
    }
    defer cm.releaseCacheLock(lock)

    targetInCache := filepath.Join(cachePath, filepath.Base(localPath))

    if dirExists(targetInCache) {
        return nil
    }

    if err := os.MkdirAll(cachePath, 0755); err != nil {
        return err
    }

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
```

---

## Issue 6: Hardlink Count Limits

**Severity**: Low

**Problem**: Filesystems have limits on hardlink count per inode:

| Filesystem | Max hardlinks per inode |
|------------|------------------------|
| ext4       | ~65,000               |
| APFS       | Unlimited (practical)  |
| NTFS       | 1,024                 |
| btrfs      | ~65,535               |

For large projects with many environments, popular cache entries could hit these limits.

```
# Example: 100 environments all sharing same node_modules
# Each file in node_modules gets 100 hardlinks
# Popular packages with many files → could approach limits
```

**Impact**: Very rare. Would require many environments with identical dependencies.

**Solution Options**:

1. **Monitor and warn** (Recommended)
   - Track hardlink count during restore
   - Warn user when approaching limits
   - Fall back to copy at threshold

2. **Use reflinks on supported filesystems**
   - APFS, btrfs support copy-on-write reflinks
   - No hardlink count issues
   - Platform-dependent

**Recommended Fix**:

```go
const HardlinkWarningThreshold = 50000

func (cm *CacheManager) checkHardlinkCount(path string) (uint64, error) {
    info, err := os.Stat(path)
    if err != nil {
        return 0, err
    }

    stat, ok := info.Sys().(*syscall.Stat_t)
    if !ok {
        return 0, nil
    }

    return stat.Nlink, nil
}

func (cm *CacheManager) HardlinkOrCopy(src, dst string) error {
    count, err := cm.checkHardlinkCount(src)
    if err == nil && count > HardlinkWarningThreshold {
        logger.Warn("hardlink count %d approaching limit, using copy", count)
        return copyFile(src, dst)
    }

    if err := os.Link(src, dst); err != nil {
        if isHardlinkLimitReached(err) {
            return copyFile(src, dst)
        }
        return err
    }

    return nil
}
```

---

## Summary Table

| Issue | Severity | Root Cause | Recommended Fix |
|-------|----------|------------|-----------------|
| Incremental fingerprints | High | Cargo stores absolute paths | Clean `.fingerprint/` after restore |
| node_modules paths | Medium | npm stores absolute paths in .bin/ | Delete `.bin/`, run npm rebuild |
| Corruption propagation | Medium | Hardlinks share data | Checksum verification |
| Stale cache from root | Low | Root built before lockfile update | Fingerprint cleaning handles it |
| Concurrent sync race | Medium | Non-atomic check-then-move | File locking |
| Hardlink count limits | Low | Filesystem limits | Monitor and fall back to copy |

## Implementation Priority

1. **Must have before Phase 2**:
   - Issue #1: Fingerprint cleaning (without this, caching provides little benefit)
   - Issue #2: node_modules .bin/ handling

2. **Should have**:
   - Issue #5: File locking for concurrent sync
   - Issue #3: Checksum verification (can add after initial release)

3. **Nice to have**:
   - Issue #6: Hardlink count monitoring (very edge case)
   - Issue #4: Already handled by #1

## Testing Recommendations

1. **Fingerprint cleaning**:
   - Build in env1, cache, restore to env2
   - Verify incremental build works (not full rebuild)
   - Measure rebuild time with/without fingerprint cleaning

2. **node_modules handling**:
   - Install deps in env1, cache, restore to env2
   - Verify `npx eslint --version` works
   - Verify `npm run build` works

3. **Concurrent sync**:
   - Run `mono sync` in parallel from two terminals
   - Verify no errors, cache created correctly

4. **Corruption detection**:
   - Manually corrupt a cached .rlib file
   - Verify restore detects and reports the corruption
