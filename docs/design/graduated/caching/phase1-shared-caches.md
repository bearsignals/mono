# Phase 1: Compilation Cache (sccache)

## Overview

This phase enables sccache for Rust compilation caching. This is a low-effort, high-impact change that significantly speeds up repeated builds.

**Expected outcome**: Rust compilation artifacts are cached and shared globally. Repeated builds are faster.

**What about download caches?** Cargo, npm, yarn, and pnpm already cache downloads globally by default (`~/.cargo`, `~/.npm`, etc.). We don't need to configure anything - it just works.

## Design Decision: Use sccache's Default Location

Mono enables sccache by setting `RUSTC_WRAPPER=sccache`. sccache uses its own default cache location:
- macOS: `~/Library/Caches/Mozilla.sccache`
- Linux: `~/.cache/sccache`

```
~/.mono/
├── state.db              # existing
├── mono.log              # existing
├── data/                 # existing
└── cache_local/          # Phase 2: per-project artifact cache
```

**Why not configure SCCACHE_DIR?**
- sccache's default location works well
- Avoids complexity of managing custom paths
- Users can override SCCACHE_DIR themselves if needed

**Why not configure CARGO_HOME/npm?**
- Download caches already work globally by default
- Changing CARGO_HOME breaks access to user's `cargo install`ed binaries
- sccache is not enabled by default - we add value by enabling it

## Files to Modify

| File | Changes |
|------|---------|
| `internal/mono/config.go` | Add `Build` config section |
| `internal/mono/operations.go` | Initialize cache and inject env vars in `Init()` |
| `internal/mono/cache.go` | **New file** - Cache utilities |

## Implementation Steps

### Step 1: Add Config Schema

**File**: `internal/mono/config.go`

```go
type BuildConfig struct {
    Sccache *bool `yaml:"sccache"` // default: auto-detect
}

type Config struct {
    Scripts ScriptsConfig `yaml:"scripts"`
    Build   BuildConfig   `yaml:"build"`
}
```

### Step 2: Create Cache Module

**File**: `internal/mono/cache.go` (new)

```go
package mono

import (
    "os"
    "os/exec"
    "path/filepath"
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

func GetMonoHome() (string, error) {
    home, err := os.UserHomeDir()
    if err != nil {
        return "", err
    }
    return filepath.Join(home, ".mono"), nil
}

func (cm *CacheManager) detectSccache() bool {
    _, err := exec.LookPath("sccache")
    return err == nil
}

func (cm *CacheManager) EnsureDirectories() error {
    return nil
}

func (cm *CacheManager) EnvVars(cfg BuildConfig) []string {
    var vars []string

    if cm.shouldEnableSccache(cfg) {
        vars = append(vars, "RUSTC_WRAPPER=sccache")
    }

    return vars
}

func (cm *CacheManager) shouldEnableSccache(cfg BuildConfig) bool {
    if cfg.Sccache != nil {
        return *cfg.Sccache && cm.SccacheAvailable
    }
    return cm.SccacheAvailable
}
```

### Step 3: Integrate into Environment Creation

**File**: `internal/mono/operations.go`

Modify the `Init()` function to initialize cache and inject env vars:

```go
func Init(path string) error {
    // ... existing code (derive names, create logger, etc.) ...

    cm, err := NewCacheManager()
    if err != nil {
        return fmt.Errorf("failed to initialize cache: %w", err)
    }

    if err := cm.EnsureDirectories(); err != nil {
        return fmt.Errorf("failed to create cache directories: %w", err)
    }

    if cm.SccacheAvailable {
        logger.Info("sccache detected, compilation caching enabled")
    } else {
        logger.Info("sccache not found, compilation caching disabled")
        logger.Info("hint: install sccache for faster builds: cargo install sccache")
    }

    cacheEnvVars := cm.EnvVars(cfg.Build)

    // Pass cacheEnvVars to runScript()
    if cfg.Scripts.Init != "" {
        if err := runScript(cfg.Scripts.Init, env, cacheEnvVars, logger); err != nil {
            return err
        }
    }

    // ... rest of init ...
}
```

### Step 4: Update Script Execution

**File**: `internal/mono/operations.go`

Modify `runScript()` to accept additional env vars:

```go
func runScript(script string, env MonoEnv, extraEnvVars []string, logger *EnvLogger) error {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
    defer cancel()

    cmd := exec.CommandContext(ctx, "bash", "-c", script)
    cmd.Dir = env.EnvPath

    cmd.Env = append(os.Environ(), env.ToEnvSlice()...)
    cmd.Env = append(cmd.Env, extraEnvVars...)

    // ... rest unchanged ...
}
```

## Testing

### Manual Test Plan

1. **Create environment with sccache**
   ```bash
   mono init ./env1
   # Should see: "sccache detected, compilation caching enabled"
   ```

2. **Verify env vars are set**
   ```yaml
   # In mono.yml:
   scripts:
     init: |
       echo "RUSTC_WRAPPER=$RUSTC_WRAPPER"
   ```
   ```bash
   mono init ./test-env
   # Output should show:
   # RUSTC_WRAPPER=sccache
   ```

3. **Verify sccache works**
   ```bash
   mono init ./env1
   cd env1 && cargo build
   sccache -s  # check stats, should show cache misses
   cargo clean && cargo build
   sccache -s  # should show cache hits
   ```

4. **Cross-environment cache sharing**
   ```bash
   mono init ./env2
   cd env2 && cargo build
   sccache -s  # should show cache hits from env1's build
   ```

5. **Test without sccache**
   ```bash
   # Temporarily rename sccache binary
   sudo mv /usr/local/bin/sccache /usr/local/bin/sccache.bak
   mono init ./env3
   # Should see: "sccache not found, compilation caching disabled"
   sudo mv /usr/local/bin/sccache.bak /usr/local/bin/sccache
   ```

6. **Disable sccache via config**
   ```yaml
   # mono.yml
   build:
     sccache: false
   ```
   ```bash
   mono init ./env4
   # Should NOT set RUSTC_WRAPPER
   ```

7. **Verify download caches work by default**
   ```bash
   # No mono config needed - these just work
   mono init ./env1
   cd env1 && cargo build  # downloads crates to ~/.cargo
   mono init ./env2
   cd env2 && cargo build  # reuses crates from ~/.cargo (no re-download)
   ```

## Acceptance Criteria

- [ ] `RUSTC_WRAPPER=sccache` injected into scripts when sccache available
- [ ] sccache auto-detected via `exec.LookPath`
- [ ] sccache can be disabled via `build.sccache: false` in config
- [ ] `sccache -s` shows cache hits for repeated builds
- [ ] Cache shared across environments and projects
- [ ] Helpful hint shown when sccache not installed
- [ ] Download caches (cargo, npm) work by default without any mono configuration

## Rollback Plan

If issues arise, sccache can be disabled per-project:

```yaml
build:
  sccache: false
```

Or globally by clearing sccache's default cache:

```bash
sccache --stop-server
rm -rf ~/Library/Caches/Mozilla.sccache  # macOS
rm -rf ~/.cache/sccache                   # Linux
```
