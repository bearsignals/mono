# Phase 1: Shared Download & Compilation Cache

## Overview

This phase implements Layer 1 (shared download cache) and Layer 2 (sccache compilation cache). These are low-effort, high-impact changes that require minimal code modifications.

**Expected outcome**: Downloads happen once across all environments and projects. Rust compilation artifacts are cached and shared globally.

## Design Decision: Global Cache in `~/.mono/cache_global/`

All global caches (Layers 1 & 2) live in `~/.mono/cache_global/`:

```
~/.mono/
├── state.db              # existing
├── mono.log              # existing
├── data/                 # existing
└── cache_global/         # Layers 1 & 2: shared across ALL projects
    ├── cargo/            # Layer 1: shared CARGO_HOME
    ├── npm/              # Layer 1: shared npm cache
    ├── yarn/             # Layer 1: shared yarn cache
    ├── pnpm/             # Layer 1: shared pnpm home
    └── sccache/          # Layer 2: compilation cache
```

**Benefits:**
- No `.gitignore` management needed
- Transparent to users (invisible in project directory)
- Consistent with existing mono state location
- Cache shared across all projects (download caches are inherently shareable)
- Survives project deletion

## Files to Modify

| File | Changes |
|------|---------|
| `internal/mono/config.go` | Add `Build` config section |
| `internal/mono/env.go` | Add cache env vars to `ToEnvSlice()` |
| `internal/mono/operations.go` | Initialize cache and inject env vars in `Init()` |
| `internal/mono/cache.go` | **New file** - Cache utilities |

## Implementation Steps

### Step 1: Add Config Schema

**File**: `internal/mono/config.go`

Add a new `Build` section to the Config struct:

```go
type BuildConfig struct {
    Strategy      string `yaml:"strategy"`       // layered|compile|download|none
    DownloadCache bool   `yaml:"download_cache"` // default: true
    Sccache       *bool  `yaml:"sccache"`        // default: auto-detect
}

type Config struct {
    Scripts ScriptsConfig `yaml:"scripts"`
    Build   BuildConfig   `yaml:"build"`
}
```

Add a method to apply defaults:

```go
func (c *Config) ApplyDefaults() {
    if c.Build.Strategy == "" {
        c.Build.Strategy = "layered"
    }
    c.Build.DownloadCache = true
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
    GlobalCacheDir   string
    CargoHome        string
    NpmCache         string
    YarnCache        string
    PnpmHome         string
    SccacheDir       string
    SccacheAvailable bool
}

func NewCacheManager() (*CacheManager, error) {
    homeDir, err := GetMonoHome()
    if err != nil {
        return nil, err
    }

    globalCacheDir := filepath.Join(homeDir, "cache_global")

    cm := &CacheManager{
        HomeDir:        homeDir,
        GlobalCacheDir: globalCacheDir,
        CargoHome:      filepath.Join(globalCacheDir, "cargo"),
        NpmCache:       filepath.Join(globalCacheDir, "npm"),
        YarnCache:      filepath.Join(globalCacheDir, "yarn"),
        PnpmHome:       filepath.Join(globalCacheDir, "pnpm"),
        SccacheDir:     filepath.Join(globalCacheDir, "sccache"),
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
    dirs := []string{
        cm.CargoHome,
        cm.NpmCache,
        cm.YarnCache,
        cm.PnpmHome,
    }

    for _, dir := range dirs {
        if err := os.MkdirAll(dir, 0755); err != nil {
            return err
        }
    }

    if cm.SccacheAvailable {
        if err := os.MkdirAll(cm.SccacheDir, 0755); err != nil {
            return err
        }
    }

    return nil
}

func (cm *CacheManager) EnvVars(cfg BuildConfig) []string {
    if cfg.Strategy == "none" {
        return nil
    }

    vars := []string{
        "CARGO_HOME=" + cm.CargoHome,
        "npm_config_cache=" + cm.NpmCache,
        "YARN_CACHE_FOLDER=" + cm.YarnCache,
        "PNPM_HOME=" + cm.PnpmHome,
    }

    if cfg.Strategy != "download" && cm.shouldEnableSccache(cfg) {
        vars = append(vars,
            "RUSTC_WRAPPER=sccache",
            "SCCACHE_DIR="+cm.SccacheDir,
        )
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

1. **Create first environment**
   ```bash
   mono init ./env1
   # Should see: "sccache detected, compilation caching enabled"
   # Verify directories: ls ~/.mono/cache_global/
   #   - cargo/
   #   - npm/
   #   - sccache/
   ```

2. **Verify env vars are set**
   ```yaml
   # In mono.yml:
   scripts:
     init: |
       echo "CARGO_HOME=$CARGO_HOME"
       echo "npm_config_cache=$npm_config_cache"
       echo "RUSTC_WRAPPER=$RUSTC_WRAPPER"
   ```
   ```bash
   mono init ./test-env
   # Output should show paths like:
   # CARGO_HOME=/Users/you/.mono/cache_global/cargo
   # npm_config_cache=/Users/you/.mono/cache_global/npm
   # RUSTC_WRAPPER=sccache
   ```

3. **Create second environment, verify cache reuse**
   ```bash
   mono init ./env2
   # ~/.mono/cache_global/cargo/registry should already contain downloaded crates
   # sccache should show cache hits
   sccache -s  # check stats
   ```

4. **Cross-project cache sharing**
   ```bash
   cd ~/other-project
   mono init ./env1
   # Should reuse crates already downloaded from first project
   ```

5. **Test without sccache**
   ```bash
   # Temporarily rename sccache binary
   sudo mv /usr/local/bin/sccache /usr/local/bin/sccache.bak
   mono init ./env3
   # Should see: "sccache not found, compilation caching disabled"
   sudo mv /usr/local/bin/sccache.bak /usr/local/bin/sccache
   ```

6. **Test strategy=none**
   ```yaml
   # mono.yml
   build:
     strategy: none
   ```
   ```bash
   mono init ./env4
   # Should NOT inject CARGO_HOME or other cache vars
   ```

## Acceptance Criteria

- [ ] `~/.mono/cache_global/cargo`, `~/.mono/cache_global/npm`, etc. directories created on first `mono init`
- [ ] `CARGO_HOME`, `npm_config_cache`, `YARN_CACHE_FOLDER`, `PNPM_HOME` injected into scripts
- [ ] sccache auto-detected and `RUSTC_WRAPPER`, `SCCACHE_DIR` set when available
- [ ] Cache disabled when `build.strategy: none` in config
- [ ] Second environment creation reuses downloaded dependencies
- [ ] Cache shared across different projects
- [ ] `sccache -s` shows cache hits for repeated builds
- [ ] No `.gitignore` changes required in projects

## Rollback Plan

If issues arise, the cache can be disabled per-project with:

```yaml
build:
  strategy: none
```

Or globally by removing the cache directories:

```bash
rm -rf ~/.mono/cache_global
```
