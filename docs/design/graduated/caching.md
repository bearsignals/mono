# Build Cache for Mono Environments

## Problem Statement

When creating multiple mono environments (via git worktrees), each environment currently requires a full copy of build artifacts (`target/` for Rust, `node_modules/` for Node.js). This results in:

1. **Slow environment creation**: Copying multi-gigabyte directories takes minutes
2. **Wasted disk space**: Identical artifacts duplicated across environments
3. **Redundant builds**: Running `cargo build` / `npm install` for unchanged dependencies

Most environments share the same dependencies (same `Cargo.lock` / `package-lock.json`), making this duplication unnecessary.

## Goals

- Reduce environment creation time from minutes to seconds for cache hits
- Share identical build artifacts across environments to save disk space
- Support environments with different dependency versions (isolation when needed)
- Work out-of-the-box with zero configuration (sensible defaults)
- Allow power users to customize caching behavior

## Non-Goals

- Cross-project cache sharing (cache is per-project)
- Remote/distributed caching
- Caching source code (handled by git worktrees)

## Design Overview: Two-Layer Caching

The caching system operates in two complementary layers:

```
┌─────────────────────────────────────────────────────────────────┐
│  Layer 2: Artifact Cache (Hardlinks)                            │
│  - Caches entire target/ and node_modules/ directories          │
│  - Instant environment creation on cache hit                    │
│  - Keyed by lockfile + toolchain version                        │
├─────────────────────────────────────────────────────────────────┤
│  Layer 1: Compilation Cache (sccache)                           │
│  - Caches individual compiled units (.rlib files)               │
│  - Works even when lockfiles differ (shared deps hit cache)     │
│  - Uses sccache's default location                              │
└─────────────────────────────────────────────────────────────────┘
```

**Why two layers?**

| Scenario                             | Layer 1 (sccache) | Layer 2 (Hardlinks)    |
| ------------------------------------ | ----------------- | ---------------------- |
| Same lockfile, same source           | Skip compile      | Hardlink (instant)     |
| Same lockfile, different source      | Skip dep compile  | Hardlink + incremental |
| Different lockfile, some shared deps | Partial cache hit | Cache miss, rebuild    |
| Completely different deps            | Cache miss        | Cache miss             |

Layer 2 (hardlinks) is the fastest but requires identical lockfiles. Layer 1 (sccache) helps when lockfiles differ but share common dependencies.

## Directory Structure

```
~/.mono/
└── cache_local/                      # artifact cache (Layer 2)
    └── <project-id>/                 # 12-char hash of root path
        ├── cargo/
        │   └── <cache-key>/          # 16-char hash of lockfile + rustc version
        │       └── target/
        ├── npm-web/                  # artifact name includes subdirectory
        │   └── <cache-key>/
        │       └── node_modules/
        └── yarn/
            └── <cache-key>/
                └── node_modules/

# sccache uses its default location (Layer 1):
# macOS: ~/Library/Caches/Mozilla.sccache
# Linux: ~/.cache/sccache
```

**Project ID**: First 12 characters of `sha256(root_path)`. Ensures cache isolation per project.

**Cache Key**: First 16 characters of `sha256(lockfile_contents + toolchain_version)`.

## Layer 1: Compilation Cache (sccache)

Mono enables sccache for Rust compilation when available:

```bash
export RUSTC_WRAPPER="sccache"
```

sccache uses its default cache location (`~/Library/Caches/Mozilla.sccache` on macOS, `~/.cache/sccache` on Linux).

**How sccache works**:

1. Intercepts rustc invocations
2. Computes hash of: source files + compiler flags + dependency artifacts
3. On cache hit: returns cached `.rlib` file immediately
4. On cache miss: runs rustc, stores result in cache

```
Env 1: cargo build
  └─ rustc serde@1.0.193 → hash: abc123 → MISS → compile → cache
  └─ rustc my_crate → hash: def456 → MISS → compile → cache

Env 2: cargo build (different worktree, same Cargo.lock)
  └─ rustc serde@1.0.193 → hash: abc123 → HIT → instant
  └─ rustc my_crate → hash: xyz789 → MISS → compile (different source)
```

**Configuration**: Enabled by default if sccache is installed. Can be disabled:

```yaml
build:
  sccache: false
```

## Layer 2: Artifact Cache (Hardlinks)

The most aggressive optimization. Caches entire `target/` and `node_modules/` directories, keyed by lockfile hash.

### Cache Key Computation

The cache key is computed by hashing:
1. Contents of key files (e.g., `Cargo.lock`, `package-lock.json`)
2. Output of key commands (e.g., `rustc --version`, `node --version`)

```go
key = sha256(
    file_contents(Cargo.lock) +
    output_of("rustc --version")
)[:16]
```

Source files are intentionally excluded. Cargo/npm handle source-level incremental compilation internally. The cache key only captures "are the dependencies the same?"

### Artifact Filtering (Cargo)

When seeding cache from existing artifacts, mono skips files that are:
- Redundant (embedded in other files)
- Path-dependent (won't work in new location)
- Build locks (indicate in-progress build)

Filtered paths for cargo artifacts:
- `*.o` files - object files embedded in `.rlib`
- `*.d` files - dependency tracking, not needed
- `incremental/` directory - contains absolute paths
- `.cargo-lock` file - build lock file

This typically reduces cache size by 30-40% compared to the full `target/` directory.

### Post-Restore Fixes

After restoring artifacts from cache, mono applies fixes to ensure builds work correctly:

**Cargo**: Touches all `dep-*` files in `.fingerprint/` directories to update their modification times. This prevents cargo from unnecessarily rebuilding dependencies due to timestamp mismatches.

**Node.js**: Removes the `.bin/` directory from `node_modules/`. Symlinks in `.bin/` contain absolute paths that won't work in the new location. npm/yarn/pnpm regenerate these on next install.

### Hardlink Mechanics

Environments with identical dependencies share the same cached artifacts via hardlinks.

```
hardlink_tree(source_dir, target_dir):
    for each file in source_dir (recursive):
        if is_directory(file):
            mkdir(target_dir/relative_path)
        else:
            hardlink(source_dir/file, target_dir/relative_path)
```

Hardlinks share the same inode. When a program modifies a hardlinked file:

1. The filesystem breaks the link (only for that file)
2. A new copy is created with the modifications
3. Other hardlinks remain unchanged

This gives us copy-on-write behavior without explicit COW filesystem support.

### Cache Storage Flow

**On cache miss:**

```
1. Run build script (cargo build, npm install)
2. Compute cache key from lockfiles
3. Move artifacts to cache:
   mv env/target ~/.mono/cache_local/<project>/<artifact>/<key>/target
4. Hardlink back to environment:
   hardlink_tree(cache/target, env/target)
```

**On cache hit:**

```
1. Compute cache key
2. Find matching cache entry
3. Hardlink to environment:
   hardlink_tree(cache/target, env/target)
4. Apply post-restore fixes
5. Run build script (handles source-level changes incrementally)
```

### Seed from Root

When creating a new environment, mono can seed the cache from the root project's existing artifacts. This happens automatically when:

1. Root project has build artifacts (e.g., `target/`)
2. Environment's lockfile matches root's lockfile
3. No build is currently in progress in root

This avoids the initial cold-cache penalty when the root project is already built.

## Auto-Detection

Mono automatically detects artifacts to cache by scanning for lockfiles:

| Lockfile | Artifact Name | Cached Directory | Key Command |
|----------|---------------|------------------|-------------|
| `Cargo.lock` | `cargo` | `target/` | `rustc --version` |
| `package-lock.json` | `npm` | `node_modules/` | `node --version` |
| `yarn.lock` | `yarn` | `node_modules/` | `node --version` |
| `pnpm-lock.yaml` | `pnpm` | `node_modules/` | `node --version` |
| `bun.lock` / `bun.lockb` | `bun` | `node_modules/` | `bun --version` |

Lockfiles in subdirectories are also detected. For example, `web/package-lock.json` creates an artifact named `npm-web` caching `web/node_modules/`.

Directories skipped during detection: `node_modules`, `target`, `.git`, `vendor`, `dist`, `build`, `.next`, `.nuxt`.

## Configuration

### Zero-Config Default (Recommended)

Mono works out-of-the-box with no cache configuration:

```yaml
scripts:
  init: |
    cargo build
    cd web && npm install

  run: |
    cargo run --bin myapp &
```

Mono automatically:
- Enables sccache if installed (Layer 1)
- Detects lockfiles and enables artifact caching (Layer 2)

### Custom Artifact Configuration

For fine-grained control:

```yaml
build:
  sccache: true  # default: true if sccache installed

  artifacts:
    - name: cargo
      key_files: [Cargo.lock]
      key_commands: ["rustc --version"]
      paths: [target]

    - name: npm-web
      key_files: [web/package-lock.json]
      key_commands: ["node --version"]
      paths: [web/node_modules]
```

## Concurrency Safety

Both caching layers are designed for concurrent access:

| Layer     | Mechanism              | Concurrent Safety |
| --------- | ---------------------- | ----------------- |
| sccache   | Internal locking       | Safe              |
| Hardlinks | COW semantics + flock  | Safe              |

**Cache locking**: When storing artifacts to cache, mono acquires an exclusive file lock (`flock`) on `<cache-path>.lock`. If another process holds the lock, the operation is skipped (assumes another process is handling it).

**Build detection**: Before seeding from root, mono checks for `.cargo-lock` file which indicates a cargo build is in progress. If detected, seeding is skipped.

Multiple environments can run `cargo build` simultaneously without conflicts:
- They share compiled dependency units via sccache (Layer 1)
- They have isolated working copies via hardlinks (Layer 2)

## Lifecycle

```
┌─────────────────────────────────────────────────────────────────┐
│                    Environment Creation (mono init)             │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  1. git worktree add                                            │
│          │                                                      │
│          ▼                                                      │
│  2. compute artifact cache keys                                 │
│          │                                                      │
│          ▼                                                      │
│  3. check for cache hits                                        │
│          │                                                      │
│          ├─── all hits ──► restore from cache, apply fixes      │
│          │                                                      │
│          └─── any miss ──► seed from root (if available)        │
│                   │                                             │
│                   ▼                                             │
│          4. re-check cache (seed may have populated it)         │
│                   │                                             │
│                   ├─── hit ──► restore from cache               │
│                   │                                             │
│                   └─── miss ──► continue without cache          │
│          │                                                      │
│          ▼                                                      │
│  5. inject RUSTC_WRAPPER=sccache (if available)                 │
│          │                                                      │
│          ▼                                                      │
│  6. run init script                                             │
│          │                                                      │
│          ▼                                                      │
│  7. store new artifacts to cache (for misses)                   │
│          │                                                      │
│          ▼                                                      │
│  8. run setup script                                            │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

## Edge Cases

**1. Corrupted cache entry**

If a cache entry is corrupted (partial write, disk error), delete it manually:

```bash
rm -rf ~/.mono/cache_local/<project-id>/<artifact>/<key>
```

The next environment creation will rebuild and re-cache.

**2. Lockfile changes mid-session**

If a user modifies `Cargo.lock` in an environment:
- Current environment continues using its hardlinked artifacts
- Next `mono init` computes a new hash
- If new hash exists in cache: restore from cache
- If not: build and cache with new key

**3. Multiple environments modifying same cached files**

Not possible. Hardlinks provide isolation:
- Env A and Env B both link to cached file X
- Env A's cargo build modifies X
- Filesystem creates a new file for Env A, breaks the hardlink
- Env B still sees original X

**4. Filesystem doesn't support hardlinks**

Fall back to regular copy. This happens automatically when `os.Link()` returns an error.

**5. Cache directory on different filesystem**

Hardlinks don't work across filesystems. Mono detects "cross-device link" errors and falls back to copying.

**6. sccache not installed**

Layer 1 is skipped. Layer 2 (hardlinks) still provides significant benefits.

**7. Build in progress**

If `.cargo-lock` exists in the source directory, seeding is skipped to avoid copying incomplete artifacts.

## Performance Characteristics

| Operation      | No Caching | sccache Only | Both Layers (hit) |
| -------------- | ---------- | ------------ | ----------------- |
| Compile deps   | Full       | Skip         | Skip              |
| Link artifacts | N/A        | N/A          | Instant           |
| Compile source | Full       | Full         | Incremental       |

Typical improvements:
- Environment creation: **minutes → seconds** (on full cache hit)
- Disk usage: **~60-80% reduction** (via hardlinks + filtering)
- Build time: **60-90% reduction** (via sccache, even on artifact cache miss)

## Alternatives Considered

### Shared CARGO_TARGET_DIR

All worktrees share a single `target/` directory via `CARGO_TARGET_DIR` environment variable.

**Rejected because:**
- Cargo's incremental compilation thrashes when switching between different source states
- No isolation between environments
- Confusing behavior when running multiple environments simultaneously

### Copy-on-Write filesystem (APFS clones)

Use `cp -c` on macOS for instant COW copies.

**Rejected because:**
- Platform-specific (macOS only)
- Requires same filesystem
- Hardlinks achieve similar benefits more portably

### Symlinks instead of hardlinks

Symlink entire `target/` directory to cache.

**Rejected because:**
- All environments would share the exact same directory
- No isolation: one environment's build affects others
- Can't support environments with different dependency versions
