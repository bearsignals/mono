# Phase 2: Artifact Cache (Hardlinks)

## Overview

This phase implements artifact caching with hardlinks. This is the most impactful optimization, enabling instant environment creation when dependencies haven't changed.

**Expected outcome**: Environments with identical lockfiles share build artifacts via hardlinks. Environment creation goes from minutes to seconds on cache hit.

## Scope

This document covers the **core** artifact cache implementation. The following are implemented as incremental additions:

- **[Phase 2a: Sync](./sync.md)** - Syncing artifacts after dependency changes
- **[Phase 2b: Seed](./seed.md)** - Populating cache from existing project root artifacts

## Prerequisites

- Phase 1 completed (sccache compilation cache)

## Design Decision: Project-Namespaced Cache in `~/.mono/cache_local/`

Artifact caches are stored in `~/.mono/cache_local/` and namespaced by project. Unlike sccache (which is globally shareable and uses its own default location), `target/` and `node_modules/` contents depend on project structure.

```
~/.mono/
└── cache_local/            # per-project artifact cache
    └── <project-id>/
        ├── cargo/
        │   └── <lockfile-hash>/
        │       └── target/
        └── npm/
            └── <lockfile-hash>/
                └── node_modules/
```

**Project ID**: Short hash of the project's root path. Simple and deterministic.

**Why per-project in `cache_local`?**
- `target/` contains project-specific compilation artifacts
- Cargo's incremental compilation uses absolute paths in fingerprints
- Avoids subtle bugs from cross-project artifact mixing

## Related Documents

- **[flaws.md](./flaws.md)** - Analysis of edge cases and potential issues with hardlink caching

## Files to Modify

| File | Changes |
|------|---------|
| `internal/mono/config.go` | Add `Artifacts` config section |
| `internal/mono/cache.go` | Add project ID, cache key computation, hardlink operations, post-restore fixes |
| `internal/mono/operations.go` | Integrate cache restore/store into `Init()` |

## Implementation Steps

### Step 1: Extend Config Schema

**File**: `internal/mono/config.go`

```go
type ArtifactConfig struct {
    Name        string   `yaml:"name"`
    KeyFiles    []string `yaml:"key_files"`
    KeyCommands []string `yaml:"key_commands"`
    Paths       []string `yaml:"paths"`
}

type BuildConfig struct {
    Sccache   *bool            `yaml:"sccache"`
    Artifacts []ArtifactConfig `yaml:"artifacts"`
}

func (c *Config) ApplyDefaults(envPath string) {
    if len(c.Build.Artifacts) == 0 {
        c.Build.Artifacts = detectArtifacts(envPath)
    }
}

func detectArtifacts(envPath string) []ArtifactConfig {
    var artifacts []ArtifactConfig

    if fileExists(filepath.Join(envPath, "Cargo.lock")) {
        artifacts = append(artifacts, ArtifactConfig{
            Name:        "cargo",
            KeyFiles:    []string{"Cargo.lock"},
            KeyCommands: []string{"rustc --version"},
            Paths:       []string{"target"},
        })
    }

    if fileExists(filepath.Join(envPath, "package-lock.json")) {
        artifacts = append(artifacts, ArtifactConfig{
            Name:        "npm",
            KeyFiles:    []string{"package-lock.json"},
            KeyCommands: []string{"node --version"},
            Paths:       []string{"node_modules"},
        })
    }

    if fileExists(filepath.Join(envPath, "yarn.lock")) {
        artifacts = append(artifacts, ArtifactConfig{
            Name:        "yarn",
            KeyFiles:    []string{"yarn.lock"},
            KeyCommands: []string{"node --version"},
            Paths:       []string{"node_modules"},
        })
    }

    if fileExists(filepath.Join(envPath, "pnpm-lock.yaml")) {
        artifacts = append(artifacts, ArtifactConfig{
            Name:        "pnpm",
            KeyFiles:    []string{"pnpm-lock.yaml"},
            KeyCommands: []string{"node --version"},
            Paths:       []string{"node_modules"},
        })
    }

    return artifacts
}
```

### Step 2: Extend CacheManager with Project Support

**File**: `internal/mono/cache.go`

Extend the CacheManager from Phase 1 with project support:

```go
import (
    "crypto/sha256"
    "encoding/hex"
    "io"
    "os/exec"
    "strings"
)

type CacheManager struct {
    HomeDir          string
    LocalCacheDir    string
    SccacheAvailable bool
}

func NewCacheManager() (*CacheManager, error) {
    homeDir, err := GetMonoHome()
    if err != nil {
        return nil, err
    }

    cm := &CacheManager{
        HomeDir:       homeDir,
        LocalCacheDir: filepath.Join(homeDir, "cache_local"),
    }

    cm.SccacheAvailable = cm.detectSccache()

    return cm, nil
}

func ComputeProjectID(rootPath string) string {
    h := sha256.Sum256([]byte(rootPath))
    return hex.EncodeToString(h[:])[:12]
}

func (cm *CacheManager) GetProjectCacheDir(rootPath string) string {
    projectID := ComputeProjectID(rootPath)
    return filepath.Join(cm.LocalCacheDir, projectID)
}
```

### Step 3: Cache Key Computation

**File**: `internal/mono/cache.go`

```go
type ArtifactCacheEntry struct {
    Name      string
    Key       string
    CachePath string
    EnvPaths  []string
    Hit       bool
}

func (cm *CacheManager) ComputeCacheKey(artifact ArtifactConfig, envPath string) (string, error) {
    h := sha256.New()

    for _, keyFile := range artifact.KeyFiles {
        fullPath := filepath.Join(envPath, keyFile)
        f, err := os.Open(fullPath)
        if err != nil {
            if os.IsNotExist(err) {
                continue
            }
            return "", fmt.Errorf("failed to read key file %s: %w", keyFile, err)
        }
        _, err = io.Copy(h, f)
        f.Close()
        if err != nil {
            return "", fmt.Errorf("failed to hash key file %s: %w", keyFile, err)
        }
    }

    for _, cmd := range artifact.KeyCommands {
        output, err := exec.Command("bash", "-c", cmd).Output()
        if err != nil {
            return "", fmt.Errorf("failed to run key command %s: %w", cmd, err)
        }
        h.Write(output)
    }

    return hex.EncodeToString(h.Sum(nil))[:16], nil
}

func (cm *CacheManager) GetArtifactCachePath(rootPath, artifactName, key string) string {
    projectCacheDir := cm.GetProjectCacheDir(rootPath)
    return filepath.Join(projectCacheDir, artifactName, key)
}

func (cm *CacheManager) PrepareArtifactCache(artifacts []ArtifactConfig, rootPath, envPath string) ([]ArtifactCacheEntry, error) {
    var entries []ArtifactCacheEntry

    for _, artifact := range artifacts {
        key, err := cm.ComputeCacheKey(artifact, envPath)
        if err != nil {
            return nil, err
        }

        cachePath := cm.GetArtifactCachePath(rootPath, artifact.Name, key)
        hit := dirExists(cachePath)

        var envPaths []string
        for _, p := range artifact.Paths {
            envPaths = append(envPaths, filepath.Join(envPath, p))
        }

        entries = append(entries, ArtifactCacheEntry{
            Name:      artifact.Name,
            Key:       key,
            CachePath: cachePath,
            EnvPaths:  envPaths,
            Hit:       hit,
        })
    }

    return entries, nil
}
```

### Step 4: Hardlink Operations

**File**: `internal/mono/cache.go`

```go
func HardlinkTree(src, dst string) error {
    return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }

        relPath, err := filepath.Rel(src, path)
        if err != nil {
            return err
        }
        dstPath := filepath.Join(dst, relPath)

        if info.IsDir() {
            return os.MkdirAll(dstPath, info.Mode())
        }

        if err := os.Link(path, dstPath); err != nil {
            if os.IsExist(err) {
                return nil
            }
            if isHardlinkNotSupported(err) {
                return copyFile(path, dstPath)
            }
            return err
        }

        return nil
    })
}

func isHardlinkNotSupported(err error) bool {
    return strings.Contains(err.Error(), "cross-device link") ||
        strings.Contains(err.Error(), "operation not supported")
}

func copyFile(src, dst string) error {
    in, err := os.Open(src)
    if err != nil {
        return err
    }
    defer in.Close()

    out, err := os.Create(dst)
    if err != nil {
        return err
    }
    defer out.Close()

    if _, err := io.Copy(out, in); err != nil {
        return err
    }

    info, err := os.Stat(src)
    if err != nil {
        return err
    }
    return os.Chmod(dst, info.Mode())
}

func (cm *CacheManager) RestoreFromCache(entry ArtifactCacheEntry) error {
    for _, envPath := range entry.EnvPaths {
        srcPath := filepath.Join(entry.CachePath, filepath.Base(envPath))
        if !dirExists(srcPath) {
            srcPath = filepath.Join(entry.CachePath, entry.Name)
        }

        if err := os.RemoveAll(envPath); err != nil {
            return fmt.Errorf("failed to remove existing %s: %w", envPath, err)
        }

        if err := HardlinkTree(srcPath, envPath); err != nil {
            return fmt.Errorf("failed to restore cache for %s: %w", entry.Name, err)
        }

        if err := cm.ApplyPostRestoreFixes(entry.Name, envPath); err != nil {
            return fmt.Errorf("failed to apply post-restore fixes for %s: %w", entry.Name, err)
        }
    }
    return nil
}

// ApplyPostRestoreFixes handles path-dependent artifacts that break when hardlinked
// to a different location. See flaws.md for detailed analysis.
func (cm *CacheManager) ApplyPostRestoreFixes(artifactName, envPath string) error {
    switch artifactName {
    case "cargo":
        return cm.cleanCargoFingerprints(envPath)
    case "npm", "yarn", "pnpm":
        return cm.cleanNodeModulesBin(envPath)
    default:
        return nil
    }
}

// cleanCargoFingerprints removes .fingerprint directories from target/.
// Cargo stores absolute paths in fingerprints, causing full rebuilds when
// artifacts are restored to a different path. Removing fingerprints forces
// Cargo to regenerate them (fast, metadata only) while keeping compiled
// artifacts (*.rlib, *.rmeta) intact.
func (cm *CacheManager) cleanCargoFingerprints(targetDir string) error {
    fingerprintDirs := []string{
        filepath.Join(targetDir, "debug", ".fingerprint"),
        filepath.Join(targetDir, "release", ".fingerprint"),
        filepath.Join(targetDir, ".fingerprint"),
    }

    for _, dir := range fingerprintDirs {
        if dirExists(dir) {
            if err := os.RemoveAll(dir); err != nil {
                return fmt.Errorf("failed to clean fingerprints at %s: %w", dir, err)
            }
        }
    }

    return nil
}

// cleanNodeModulesBin removes the .bin directory from node_modules.
// npm/yarn/pnpm store absolute path symlinks in .bin/, which break when
// restored to a different path. The build script should run `npm rebuild`
// (or equivalent) to regenerate these symlinks.
func (cm *CacheManager) cleanNodeModulesBin(nodeModulesDir string) error {
    binDir := filepath.Join(nodeModulesDir, ".bin")
    if dirExists(binDir) {
        if err := os.RemoveAll(binDir); err != nil {
            return fmt.Errorf("failed to clean .bin at %s: %w", binDir, err)
        }
    }
    return nil
}

func (cm *CacheManager) StoreToCache(entry ArtifactCacheEntry) error {
    if err := os.MkdirAll(entry.CachePath, 0755); err != nil {
        return fmt.Errorf("failed to create cache dir: %w", err)
    }

    for _, envPath := range entry.EnvPaths {
        if !dirExists(envPath) {
            continue
        }

        cacheDst := filepath.Join(entry.CachePath, filepath.Base(envPath))

        if err := os.Rename(envPath, cacheDst); err != nil {
            return fmt.Errorf("failed to move %s to cache: %w", envPath, err)
        }

        if err := HardlinkTree(cacheDst, envPath); err != nil {
            return fmt.Errorf("failed to hardlink back from cache: %w", err)
        }
    }

    return nil
}

func dirExists(path string) bool {
    info, err := os.Stat(path)
    return err == nil && info.IsDir()
}

func fileExists(path string) bool {
    info, err := os.Stat(path)
    return err == nil && !info.IsDir()
}
```

### Step 5: Integrate into Operations

**File**: `internal/mono/operations.go`

```go
func Init(path string) error {
    // ... existing setup code ...

    cfg, err := LoadConfig(envPath)
    if err != nil {
        return err
    }
    cfg.ApplyDefaults(envPath)

    cm, err := NewCacheManager()
    if err != nil {
        return err
    }
    if err := cm.EnsureDirectories(); err != nil {
        return err
    }

    cacheEntries, err := cm.PrepareArtifactCache(cfg.Build.Artifacts, rootPath, envPath)
    if err != nil {
        logger.Warn("failed to prepare artifact cache: %v", err)
    }

    allHit := true
    for i := range cacheEntries {
        entry := &cacheEntries[i]
        if entry.Hit {
            logger.Info("cache hit for %s (key: %s)", entry.Name, entry.Key)
            if err := cm.RestoreFromCache(*entry); err != nil {
                logger.Warn("failed to restore cache: %v", err)
                entry.Hit = false
                allHit = false
            }
        } else {
            logger.Info("cache miss for %s (key: %s)", entry.Name, entry.Key)
            allHit = false
        }
    }

    cacheEnvVars := cm.EnvVars(cfg.Build)
    cacheEnvVars = append(cacheEnvVars, fmt.Sprintf("MONO_CACHE_HIT=%t", allHit))
    cacheEnvVars = append(cacheEnvVars, "MONO_CACHE_DIR="+cm.LocalCacheDir)

    if cfg.Scripts.Init != "" {
        if err := runScript(cfg.Scripts.Init, env, cacheEnvVars, logger); err != nil {
            return err
        }
    }

    for i := range cacheEntries {
        entry := &cacheEntries[i]
        if !entry.Hit {
            if err := cm.StoreToCache(*entry); err != nil {
                logger.Warn("failed to store %s to cache: %v", entry.Name, err)
            } else {
                logger.Info("stored %s to cache (key: %s)", entry.Name, entry.Key)
                entry.Hit = true
            }
        }
    }

    // ... rest of init (docker, setup script, tmux) ...
}
```

## Directory Structure After Implementation

```
~/.mono/
├── state.db
├── mono.log
├── data/
└── cache_local/            # per-project artifact cache
    └── a1b2c3d4e5f6/       # project ID (hash of /Users/x/myproject)
        ├── cargo/
        │   └── 7g8h9i0j/
        │       └── target/
        └── npm/
            └── k1l2m3n4/
                └── node_modules/

# sccache uses its default location:
# macOS: ~/Library/Caches/Mozilla.sccache
# Linux: ~/.cache/sccache

/Users/x/myproject/
└── environments/
    ├── env1/
    │   └── target/  → hardlinks to ~/.mono/cache_local/a1b2c3d4e5f6/cargo/7g8h9i0j/target
    └── env2/
        └── target/  → hardlinks to ~/.mono/cache_local/a1b2c3d4e5f6/cargo/7g8h9i0j/target
```

## Testing

### Manual Test Plan

1. **First environment - cache miss**
   ```bash
   mono init ./env1
   # Should see: "cache miss for cargo (key: a1b2c3d4)"
   # After init: "stored cargo to cache (key: a1b2c3d4)"
   # Verify: ls ~/.mono/cache_local/
   ```

2. **Second environment - cache hit**
   ```bash
   mono init ./env2
   # Should see: "cache hit for cargo (key: a1b2c3d4)"
   # Build should be nearly instant
   ```

3. **Verify hardlink sharing**
   ```bash
   ls -i env1/target/debug/deps/libserde*.rlib env2/target/debug/deps/libserde*.rlib
   # Both should show same inode number
   ```

4. **Verify COW behavior**
   ```bash
   # Modify source in env1
   echo "// change" >> env1/src/main.rs
   cd env1 && cargo build
   ls -i env1/target/debug/myapp env2/target/debug/myapp
   # env1 should have different inode now
   ```

5. **Different dependencies - new cache entry**
   ```bash
   # In env3, modify Cargo.lock
   mono init ./env3
   # Should see different cache key
   ls ~/.mono/cache_local/*/cargo/
   # Should show two hash directories
   ```

6. **Cross-filesystem fallback**
   ```bash
   # If home is on different filesystem than project
   # Should see warning and fall back to copy
   ```

7. **Different projects are isolated**
   ```bash
   cd ~/other-project
   mono init ./env1
   ls ~/.mono/cache_local/
   # Should show two project ID directories
   ```

### Unit Tests

```go
func TestComputeProjectID(t *testing.T) {
    id1 := ComputeProjectID("/Users/x/project1")
    id2 := ComputeProjectID("/Users/x/project1")
    id3 := ComputeProjectID("/Users/x/project2")

    assert.Equal(t, id1, id2)
    assert.NotEqual(t, id1, id3)
    assert.Len(t, id1, 12)
}

func TestComputeCacheKey(t *testing.T) {
    cm, _ := NewCacheManager()

    artifact := ArtifactConfig{
        Name:        "cargo",
        KeyFiles:    []string{"Cargo.lock"},
        KeyCommands: []string{"echo v1.0"},
    }

    key1, err := cm.ComputeCacheKey(artifact, "./testdata/proj1")
    require.NoError(t, err)

    key2, err := cm.ComputeCacheKey(artifact, "./testdata/proj1")
    require.NoError(t, err)
    assert.Equal(t, key1, key2)

    key3, err := cm.ComputeCacheKey(artifact, "./testdata/proj2")
    require.NoError(t, err)
    assert.NotEqual(t, key1, key3)
}

func TestHardlinkTree(t *testing.T) {
    src := t.TempDir()
    dst := filepath.Join(t.TempDir(), "dst")

    os.WriteFile(filepath.Join(src, "file.txt"), []byte("content"), 0644)
    os.MkdirAll(filepath.Join(src, "subdir"), 0755)
    os.WriteFile(filepath.Join(src, "subdir", "nested.txt"), []byte("nested"), 0644)

    err := HardlinkTree(src, dst)
    require.NoError(t, err)

    srcInfo, _ := os.Stat(filepath.Join(src, "file.txt"))
    dstInfo, _ := os.Stat(filepath.Join(dst, "file.txt"))

    srcSys := srcInfo.Sys().(*syscall.Stat_t)
    dstSys := dstInfo.Sys().(*syscall.Stat_t)
    assert.Equal(t, srcSys.Ino, dstSys.Ino)
}
```

## Post-Restore Build Script Requirements

After restoring from cache, certain artifacts need regeneration due to absolute paths. The post-restore fixes handle cleanup, but build scripts may need additional steps:

**For npm/yarn/pnpm projects**: The `.bin/` directory is removed after restore. Build scripts should include `npm rebuild` (or equivalent) to regenerate CLI symlinks:

```bash
# Example mono.yaml init script
scripts:
  init: |
    if [ "$MONO_CACHE_HIT" = "true" ]; then
      npm rebuild  # Regenerates .bin/ symlinks
    else
      npm install
    fi
```

**For Cargo projects**: The `.fingerprint/` directories are removed. No script changes needed - Cargo automatically regenerates fingerprints on next build (~5-10 seconds overhead).

## Acceptance Criteria

- [ ] Cache key computed from lockfiles + tool versions
- [ ] Auto-detection of Cargo.lock, package-lock.json, yarn.lock, pnpm-lock.yaml
- [ ] First env creation stores artifacts in `~/.mono/cache_local/<project-id>/<type>/<hash>/`
- [ ] Subsequent envs with same lockfile restore via hardlinks
- [ ] Post-restore fixes applied (fingerprint cleaning, .bin/ removal)
- [ ] Build script runs after restore for incremental source compilation
- [ ] Modifications in one env don't affect others (COW via hardlink breakage)
- [ ] Graceful fallback to copy when hardlinks not supported
- [ ] `MONO_CACHE_HIT` env var correctly set
- [ ] Different projects have isolated cache namespaces
- [ ] No files created in project directory

See [Phase 2a: Sync](./sync.md) and [Phase 2b: Seed](./seed.md) for additional acceptance criteria.

## Edge Cases

| Scenario | Behavior |
|----------|----------|
| Lockfile doesn't exist | Skip that artifact cache |
| rustc/node not installed | Key command fails, skip cache |
| Cache on different filesystem | Fall back to copy, warn user |
| Interrupted cache store | Partial entry may exist, next init rebuilds |
| Corrupted cache entry | Build will fail, user runs `mono cache clean` |
| Project path changes | New project ID, old cache orphaned |
| Cargo fingerprints have wrong paths | Cleaned after restore, Cargo regenerates |
| node_modules .bin/ has wrong symlinks | Cleaned after restore, `npm rebuild` fixes |

See [Phase 2a: Sync](./sync.md) and [Phase 2b: Seed](./seed.md) for additional edge cases.

## Performance Expectations

| Operation | Before | After (miss) | After (hit) |
|-----------|--------|--------------|-------------|
| First env | 5-10 min | 5-10 min | N/A |
| Subsequent env | 5-10 min | 30s (sccache) | <5s |
| Disk usage | 2GB × N | 2GB + ~50MB × N | 2GB + ~50MB × N |
