# Phase 2b Seed Optimization

**Status:** Proposed
**Parent:** [phase2b-seed.md](./phase2b-seed.md)

## Context

Phase 2b defines seeding as the mechanism to populate workspace caches from the root project. The design uses `HardlinkTree` for this purpose, assuming "Cache populated instantly" via hardlinks.

In practice, large directories (9.5GB, 50k files for Rust `target/`) take 60-80 minutes with sequential `os.Link()` syscalls. This document proposes optimizations to achieve the expected "instant" behavior.

## Problem

`SeedFromRoot` copies build artifacts from root project to workspace cache using `HardlinkTree`. Current implementation does sequential syscalls - one `os.Link()` per file. For large directories, this is unacceptably slow.

## Goal

Seeding should complete in seconds, not minutes. Replicas should not regenerate large build artifacts.

## Research: Cargo Artifact Analysis

Analysis of a real Rust project (bibliotek: 9.7GB, 50,087 files) to understand which files actually speed up builds.

### Files That Speed Up Builds

| File Type | Count | Size | Purpose |
|-----------|-------|------|---------|
| `.rlib` | 559 | 2.4GB | Compiled crate libraries - **essential**, without these cargo recompiles all deps |
| `.rmeta` | 1,260 | 1.2GB | Metadata for type checking - **essential**, enables pipelined compilation |
| `build/` | 1,729 | 994MB | Build script outputs (compiled C code for -sys crates) - **essential** |
| `.a` | 3 | 453MB | Static libraries - **essential** for linking |
| `.dylib` | 37 | 97MB | Dynamic libraries - **essential** for proc-macros |
| binaries | ~50 | 762MB | Compiled executables - useful |
| **Total** | ~3,638 | ~5.9GB | |

### Files That Don't Help in New Workspace

| File Type | Count | Size | Why Skip |
|-----------|-------|------|----------|
| `.o` | 39,605 | 175MB | Intermediate object files - already embedded in `.rlib`, or for local binary only |
| `.d` | 1,403 | 19MB | Makefile-style dependency tracking - not used by cargo |
| `incremental/` | 544 | 189MB | rustc's incremental cache - path-dependent, won't work in new location |
| `.fingerprint/` | - | - | Already handled by `ApplyPostRestoreFixes` (deleted after restore) |
| **Total** | ~41,552 | ~383MB | |

### Result

By skipping `.o`, `.d`, and `incremental/`:
- **93% fewer files** (3,638 vs 50,087)
- **94% of useful data retained** (5.9GB vs 6.3GB)
- Seeding time reduced proportionally

### Sources

- [Rust Compiler Dev Guide - Libraries and Metadata](https://rustc-dev-guide.rust-lang.org/backend/libs-and-metadata.html)
- [Cargo Build Cache](https://doc.rust-lang.org/cargo/reference/build-cache.html)
- [Cargo Fingerprint Module](https://doc.rust-lang.org/beta/nightly-rustc/cargo/core/compiler/fingerprint/index.html)

## Implementation

### Phase 1: Progress Logging Infrastructure

Make long operations visible regardless of speed.

- Modify `SeedFromRoot` and `seedArtifactFromRoot` to accept a logger parameter
- Before seeding, count files in source directory (quick `filepath.WalkDir` that only counts)
- Create a progress reporter that logs every 5 seconds: `"seeding cargo: 11821/50087 files (24%)"`
- Wire this through `HardlinkTree` or its replacement

### Phase 2: Skip Low-Value Paths

Reduce file count by excluding paths with poor cost/benefit ratio. Based on our research, this yields a **93% reduction in file count** while retaining 94% of useful data.

**Note:** Path-dependent artifacts (`.fingerprint`, `node_modules/.bin`) are already handled by `ApplyPostRestoreFixes` in the parent design.

- Create skip rules for cargo artifacts:
  - `**/*.o` - Intermediate object files (39k files, 175MB) - already in `.rlib`
  - `**/*.d` - Dependency tracking files (1.4k files, 19MB) - not used by cargo
  - `**/incremental/**` - rustc incremental cache (544 files, 189MB) - path-dependent
  - `target/.cargo-lock` - Lock file, not useful across workspaces
- Add a `shouldSkip(path string, artifact ArtifactConfig) bool` function
- Integrate skip logic into the walking/copying phase
- This applies to all seeding methods (parallel Go implementation)

### Phase 3: Parallel Go Implementation

System utilities (`cp`, `rsync`) are single-threaded and won't be faster than parallel Go for hardlinking. A parallel Go implementation with skip logic is the optimal approach.

- Replace sequential `filepath.Walk` + `os.Link` with parallel approach
- **Structure:**
  - Directory walker goroutine: walks tree, sends paths to channel
  - Worker pool (16-32 workers): receive paths, create hardlinks
  - Progress tracker: counts completed files, logs periodically
- **Directory handling:**
  - Collect all directories first (single walk)
  - Create directory structure sequentially (fast, can't parallelize)
  - Then parallelize only the file hardlinking
- **Error handling:**
  - Use errgroup or similar for clean cancellation
  - Fail fast on first error, cancel remaining workers

### Phase 4: Integration and Cleanup

Wire everything together in `cache.go`.

- Replace `HardlinkTree` calls in `seedToCache` with new `SeedDirectory` function
- `SeedDirectory` uses parallel Go with skip logic
- Update `RestoreFromCache` to use same optimized approach
- Remove or deprecate old `HardlinkTree` function
- Add tests for seeding

## File Changes

1. `internal/mono/cache.go`:
   - New `SeedDirectory` function with parallel workers
   - New `shouldSkipPath` function with skip rules per artifact type
   - Progress logging integration

2. `internal/mono/operations.go`:
   - Pass logger to cache seeding functions
   - Update progress log messages

3. `internal/mono/logger.go`:
   - Add `LogProgress` method for periodic updates (debounced)

## Compatibility

This optimization is transparent to the rest of the caching system:
- Same semantics as current `HardlinkTree` (hardlinks where possible, copy fallback)
- No changes to cache structure or `CacheManifest`
- No changes to `RestoreFromCache` interface (though it benefits from same optimizations)
- Maintains cross-filesystem copy fallback behavior
