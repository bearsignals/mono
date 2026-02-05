package mono

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
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

func ComputeProjectID(rootPath string) string {
	h := sha256.Sum256([]byte(rootPath))
	return hex.EncodeToString(h[:])[:12]
}

func (cm *CacheManager) GetProjectCacheDir(rootPath string) string {
	projectID := ComputeProjectID(rootPath)
	return filepath.Join(cm.LocalCacheDir, projectID)
}

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

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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

		// Check if it's a symlink - recreate as symlink instead of hardlinking target
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("failed to read symlink %s: %w", path, err)
			}
			if err := os.Symlink(target, dstPath); err != nil {
				if os.IsExist(err) {
					return nil
				}
				return fmt.Errorf("failed to create symlink %s: %w", dstPath, err)
			}
			return nil
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

func shouldSkipPath(relPath string, artifactName string) bool {
	switch artifactName {
	case "cargo":
		return shouldSkipCargoPath(relPath)
	default:
		return false
	}
}

func shouldSkipCargoPath(relPath string) bool {
	if strings.HasSuffix(relPath, ".o") {
		return true
	}
	if strings.HasSuffix(relPath, ".d") {
		return true
	}
	if strings.Contains(relPath, "/incremental/") || strings.HasPrefix(relPath, "incremental/") {
		return true
	}
	if relPath == ".cargo-lock" {
		return true
	}
	return false
}

type SeedOptions struct {
	ArtifactName    string
	Logger          *FileLogger
	NumWorkers      int
	OperationName   string
	ProgressTimeout time.Duration // Abort if no progress for this duration (0 = 30s default)
	FileTimeout     time.Duration // Timeout for individual file operations (0 = 10s default)
}

func copyDirectory(src, dst, artifactName string, logger *FileLogger, operation string) error {
	return SeedDirectory(src, dst, SeedOptions{
		ArtifactName:  artifactName,
		Logger:        logger,
		OperationName: operation,
	})
}

func countFiles(src string, artifactName string) (int64, error) {
	var count int64
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if !shouldSkipPath(relPath, artifactName) {
			count++
		}
		return nil
	})
	return count, err
}

type fileEntry struct {
	srcPath string
	dstPath string
	relPath string
	mode    fs.FileMode
}

func SeedDirectory(src, dst string, opts SeedOptions) error {
	numWorkers := opts.NumWorkers
	if numWorkers <= 0 {
		numWorkers = 16 // Reduced from 16 to avoid APFS contention
	}

	progressTimeout := opts.ProgressTimeout
	if progressTimeout <= 0 {
		progressTimeout = 30 * time.Second
	}

	fileTimeout := opts.FileTimeout
	if fileTimeout <= 0 {
		fileTimeout = 10 * time.Second
	}

	var totalFiles int64
	var progress *ProgressLogger
	if opts.Logger != nil {
		var err error
		totalFiles, err = countFiles(src, opts.ArtifactName)
		if err != nil {
			return fmt.Errorf("failed to count files: %w", err)
		}
		operation := opts.OperationName
		if operation == "" {
			operation = "seeding"
		}
		progress = NewProgressLogger(opts.Logger, operation+" "+opts.ArtifactName, totalFiles)
	}

	var dirs []struct {
		path string
		mode fs.FileMode
	}
	var files []fileEntry

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		if d.IsDir() {
			if shouldSkipPath(relPath+"/", opts.ArtifactName) {
				return filepath.SkipDir
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			dirs = append(dirs, struct {
				path string
				mode fs.FileMode
			}{filepath.Join(dst, relPath), info.Mode()})
			return nil
		}

		if shouldSkipPath(relPath, opts.ArtifactName) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		files = append(files, fileEntry{
			srcPath: path,
			dstPath: filepath.Join(dst, relPath),
			relPath: relPath,
			mode:    info.Mode(),
		})

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to walk source directory: %w", err)
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir.path, dir.mode); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir.path, err)
		}
	}

	fileChan := make(chan fileEntry, len(files))
	for _, f := range files {
		fileChan <- f
	}
	close(fileChan)

	// Track progress for timeout detection
	var lastProgress atomic.Int64
	lastProgress.Store(time.Now().UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Watchdog goroutine: cancel if no progress within timeout
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				lastTime := time.Unix(0, lastProgress.Load())
				if time.Since(lastTime) > progressTimeout {
					cancel()
					return
				}
			}
		}
	}()

	g, gctx := errgroup.WithContext(ctx)

	var once sync.Once
	var firstErr error

	for i := 0; i < numWorkers; i++ {
		g.Go(func() error {
			for {
				select {
				case <-gctx.Done():
					return gctx.Err()
				case f, ok := <-fileChan:
					if !ok {
						return nil
					}

					if err := linkOrCopyFileWithTimeout(f.srcPath, f.dstPath, fileTimeout); err != nil {
						once.Do(func() {
							firstErr = fmt.Errorf("failed to link %s: %w", f.relPath, err)
						})
						return firstErr
					}

					// Update progress timestamp
					lastProgress.Store(time.Now().UnixNano())

					if progress != nil {
						progress.Increment()
					}
				}
			}
		})
	}

	err = g.Wait()
	cancel() // Stop watchdog
	<-watchdogDone

	if err != nil {
		if err == context.Canceled {
			return fmt.Errorf("seeding timed out: no progress for %v", progressTimeout)
		}
		return err
	}

	if progress != nil {
		progress.Done()
	}

	return nil
}

func linkOrCopyFile(src, dst string) error {
	// Check if source is a symlink - we need to recreate symlinks, not hardlink their targets
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err
	}

	if srcInfo.Mode()&os.ModeSymlink != 0 {
		// It's a symlink - read the target and recreate it
		target, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("failed to read symlink %s: %w", src, err)
		}
		if err := os.Symlink(target, dst); err != nil {
			if os.IsExist(err) {
				return nil
			}
			return fmt.Errorf("failed to create symlink %s: %w", dst, err)
		}
		return nil
	}

	// Regular file - hardlink it
	if err := os.Link(src, dst); err != nil {
		if os.IsExist(err) {
			return nil
		}
		if isHardlinkNotSupported(err) {
			return copyFile(src, dst)
		}
		return err
	}
	return nil
}

// linkOrCopyFileWithTimeout wraps linkOrCopyFile with a timeout.
// If the operation doesn't complete within the timeout, it returns an error.
// Note: the underlying goroutine may still be blocked, but we move on.
func linkOrCopyFileWithTimeout(src, dst string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		done <- linkOrCopyFile(src, dst)
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("operation timed out after %v", timeout)
	}
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

func (cm *CacheManager) RestoreFromCache(entry ArtifactCacheEntry, logger *FileLogger) error {
	for _, envPath := range entry.EnvPaths {
		srcPath := filepath.Join(entry.CachePath, filepath.Base(envPath))
		if !dirExists(srcPath) {
			srcPath = filepath.Join(entry.CachePath, entry.Name)
		}

		if err := os.RemoveAll(envPath); err != nil {
			return fmt.Errorf("failed to remove existing %s: %w", envPath, err)
		}

		if err := copyDirectory(srcPath, envPath, entry.Name, logger, "restoring"); err != nil {
			return fmt.Errorf("failed to restore cache for %s: %w", entry.Name, err)
		}

		if err := cm.ApplyPostRestoreFixes(entry.Name, envPath); err != nil {
			return fmt.Errorf("failed to apply post-restore fixes for %s: %w", entry.Name, err)
		}
	}
	return nil
}

func (cm *CacheManager) ApplyPostRestoreFixes(artifactName, envPath string) error {
	switch artifactName {
	case "cargo":
		return cm.touchCargoFingerprints(envPath)
	case "npm", "yarn", "pnpm", "bun":
		return cm.cleanNodeModulesBin(envPath)
	default:
		return nil
	}
}

func (cm *CacheManager) touchCargoFingerprints(targetDir string) error {
	now := time.Now()

	for _, profile := range []string{"debug", "release"} {
		fingerprintDir := filepath.Join(targetDir, profile, ".fingerprint")
		if !dirExists(fingerprintDir) {
			continue
		}

		if err := touchDepFilesParallel(fingerprintDir, now, 8); err != nil {
			return err
		}
	}

	return nil
}

func touchDepFiles(fingerprintDir string, now time.Time) error {
	crateEntries, err := os.ReadDir(fingerprintDir)
	if err != nil {
		return err
	}

	for _, crateEntry := range crateEntries {
		if !crateEntry.IsDir() {
			continue
		}

		crateDir := filepath.Join(fingerprintDir, crateEntry.Name())
		fileEntries, err := os.ReadDir(crateDir)
		if err != nil {
			continue
		}

		for _, fileEntry := range fileEntries {
			if fileEntry.IsDir() {
				continue
			}
			if !strings.HasPrefix(fileEntry.Name(), "dep-") {
				continue
			}
			filePath := filepath.Join(crateDir, fileEntry.Name())
			if err := os.Chtimes(filePath, now, now); err != nil {
				return err
			}
		}
	}

	return nil
}

func touchDepFilesParallel(fingerprintDir string, now time.Time, numWorkers int) error {
	crateEntries, err := os.ReadDir(fingerprintDir)
	if err != nil {
		return err
	}

	if numWorkers <= 0 {
		numWorkers = 8
	}

	var depFiles []string
	for _, crateEntry := range crateEntries {
		if !crateEntry.IsDir() {
			continue
		}

		crateDir := filepath.Join(fingerprintDir, crateEntry.Name())
		fileEntries, err := os.ReadDir(crateDir)
		if err != nil {
			continue
		}

		for _, fileEntry := range fileEntries {
			if fileEntry.IsDir() {
				continue
			}
			if !strings.HasPrefix(fileEntry.Name(), "dep-") {
				continue
			}
			depFiles = append(depFiles, filepath.Join(crateDir, fileEntry.Name()))
		}
	}

	if len(depFiles) == 0 {
		return nil
	}

	fileChan := make(chan string, len(depFiles))
	for _, f := range depFiles {
		fileChan <- f
	}
	close(fileChan)

	g, ctx := errgroup.WithContext(context.Background())

	for i := 0; i < numWorkers; i++ {
		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case path, ok := <-fileChan:
					if !ok {
						return nil
					}
					if err := os.Chtimes(path, now, now); err != nil {
						return err
					}
				}
			}
		})
	}

	return g.Wait()
}

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

type SyncOptions struct {
	HardlinkBack bool
}

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
		return nil, nil
	}

	return f, nil
}

func (cm *CacheManager) releaseCacheLock(f *os.File) {
	if f != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

func (cm *CacheManager) Sync(artifacts []ArtifactConfig, rootPath, envPath string, opts SyncOptions) error {
	for _, artifact := range artifacts {
		if err := cm.syncArtifact(artifact, rootPath, envPath, opts); err != nil {
			return err
		}
	}
	return nil
}

func (cm *CacheManager) isBuildInProgress(envPath string, artifact ArtifactConfig) bool {
	switch artifact.Name {
	case "cargo":
		lockFile := filepath.Join(envPath, "target", ".cargo-lock")
		return fileExists(lockFile)
	default:
		return false
	}
}

// CargoProcessInfo contains information about a running cargo process
type CargoProcessInfo struct {
	PID     string
	Command string
}

// DetectRunningCargoProcesses checks if any cargo/rustc processes are running
// that reference the given project path. Returns a list of matching processes.
func DetectRunningCargoProcesses(projectPath string) ([]CargoProcessInfo, error) {
	// Use ps to get all cargo and rustc processes with their full command lines
	cmd := exec.Command("ps", "-eo", "pid,command")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run ps: %w", err)
	}

	var processes []CargoProcessInfo
	lines := strings.Split(string(output), "\n")

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Check if this is a cargo or rustc process
		if !strings.Contains(line, "cargo") && !strings.Contains(line, "rustc") {
			continue
		}

		// Check if it references our project path
		if !strings.Contains(line, projectPath) {
			continue
		}

		// Parse PID (first field)
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		processes = append(processes, CargoProcessInfo{
			PID:     fields[0],
			Command: strings.Join(fields[1:], " "),
		})
	}

	return processes, nil
}

// CheckCargoBuildConflicts checks for any conditions that would cause cargo build
// to block: either a .cargo-lock file or running cargo/rustc processes.
// Returns a descriptive error if a conflict is found, nil otherwise.
func CheckCargoBuildConflicts(projectPath string) error {
	// Check for .cargo-lock file
	lockFile := filepath.Join(projectPath, "target", ".cargo-lock")
	if fileExists(lockFile) {
		return fmt.Errorf("cargo lock file exists at %s - another cargo process may be running", lockFile)
	}

	// Check for running cargo/rustc processes
	processes, err := DetectRunningCargoProcesses(projectPath)
	if err != nil {
		// Don't fail on detection errors, just log and continue
		return nil
	}

	if len(processes) > 0 {
		var pids []string
		for _, p := range processes {
			pids = append(pids, p.PID)
		}
		return fmt.Errorf("found %d running cargo/rustc process(es) for %s (PIDs: %s)",
			len(processes), projectPath, strings.Join(pids, ", "))
	}

	return nil
}

func (cm *CacheManager) syncArtifact(artifact ArtifactConfig, rootPath, envPath string, opts SyncOptions) error {
	if cm.isBuildInProgress(envPath, artifact) {
		return fmt.Errorf("build in progress, cannot sync %s", artifact.Name)
	}

	key, err := cm.ComputeCacheKey(artifact, envPath)
	if err != nil {
		return fmt.Errorf("failed to compute cache key for %s: %w", artifact.Name, err)
	}

	cachePath := cm.GetArtifactCachePath(rootPath, artifact.Name, key)

	if dirExists(cachePath) {
		return nil
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
	lock, err := cm.acquireCacheLock(cachePath)
	if err != nil {
		return err
	}
	if lock == nil {
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
		if err := HardlinkTree(targetInCache, localPath); err != nil {
			recoverErr := os.Rename(targetInCache, localPath)
			cleanupErr := os.RemoveAll(cachePath)
			if recoverErr != nil {
				return fmt.Errorf("failed to hardlink back and recovery failed: %w (recovery error: %v)", err, recoverErr)
			}
			if cleanupErr != nil {
				return fmt.Errorf("failed to hardlink back, recovered but cleanup failed: %w (cleanup error: %v)", err, cleanupErr)
			}
			return fmt.Errorf("failed to hardlink back, recovered: %w", err)
		}
	}

	return nil
}

func (cm *CacheManager) copyToCache(localPath, targetInCache string, hardlinkBack bool) error {
	if err := copyDir(localPath, targetInCache); err != nil {
		return err
	}

	if hardlinkBack {
		return nil
	}

	return os.RemoveAll(localPath)
}

func isCrossDevice(err error) bool {
	return strings.Contains(err.Error(), "cross-device link") ||
		strings.Contains(err.Error(), "invalid cross-device link")
}

func copyDir(src, dst string) error {
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

		// Check if it's a symlink - recreate as symlink instead of copying target
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("failed to read symlink %s: %w", path, err)
			}
			if err := os.Symlink(target, dstPath); err != nil {
				if os.IsExist(err) {
					return nil
				}
				return fmt.Errorf("failed to create symlink %s: %w", dstPath, err)
			}
			return nil
		}

		return copyFile(path, dstPath)
	})
}

func (cm *CacheManager) SeedFromRoot(artifacts []ArtifactConfig, rootPath, envPath string, logger *FileLogger) error {
	for _, artifact := range artifacts {
		if err := cm.seedArtifactFromRoot(artifact, rootPath, envPath, logger); err != nil {
			return err
		}
	}
	return nil
}

func (cm *CacheManager) seedArtifactFromRoot(artifact ArtifactConfig, rootPath, envPath string, logger *FileLogger) error {
	if rootPath == envPath {
		return nil
	}

	envKey, err := cm.ComputeCacheKey(artifact, envPath)
	if err != nil {
		return fmt.Errorf("failed to compute cache key for env %s: %w", artifact.Name, err)
	}

	cachePath := cm.GetArtifactCachePath(rootPath, artifact.Name, envKey)
	if dirExists(cachePath) {
		return nil
	}

	rootKey, err := cm.ComputeCacheKey(artifact, rootPath)
	if err != nil {
		return fmt.Errorf("failed to compute cache key for root %s: %w", artifact.Name, err)
	}

	if envKey != rootKey {
		return nil
	}

	if cm.isBuildInProgress(rootPath, artifact) {
		return nil
	}

	for _, p := range artifact.Paths {
		rootArtifact := filepath.Join(rootPath, p)
		if !dirExists(rootArtifact) {
			continue
		}

		if err := cm.seedToCache(rootArtifact, cachePath, artifact.Name, logger); err != nil {
			return fmt.Errorf("failed to seed %s from root: %w", artifact.Name, err)
		}
	}

	return nil
}

func (cm *CacheManager) seedToCache(sourcePath, cachePath, artifactName string, logger *FileLogger) error {
	if err := os.MkdirAll(cachePath, 0755); err != nil {
		return err
	}

	targetInCache := filepath.Join(cachePath, filepath.Base(sourcePath))

	if dirExists(targetInCache) {
		return nil
	}

	err := SeedDirectory(sourcePath, targetInCache, SeedOptions{
		ArtifactName: artifactName,
		Logger:       logger,
	})
	if err != nil {
		os.RemoveAll(targetInCache)
		return err
	}
	return nil
}

type CacheSizeEntry struct {
	ProjectID string
	Artifact  string
	CacheKey  string
	Size      int64
}

func (cm *CacheManager) GetCacheSizes() ([]CacheSizeEntry, error) {
	var entries []CacheSizeEntry

	if !dirExists(cm.LocalCacheDir) {
		return entries, nil
	}

	projectDirs, err := os.ReadDir(cm.LocalCacheDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read cache directory: %w", err)
	}

	for _, projectDir := range projectDirs {
		if !projectDir.IsDir() {
			continue
		}
		projectID := projectDir.Name()
		projectPath := filepath.Join(cm.LocalCacheDir, projectID)

		artifactDirs, err := os.ReadDir(projectPath)
		if err != nil {
			continue
		}

		for _, artifactDir := range artifactDirs {
			if !artifactDir.IsDir() {
				continue
			}
			artifact := artifactDir.Name()
			artifactPath := filepath.Join(projectPath, artifact)

			keyDirs, err := os.ReadDir(artifactPath)
			if err != nil {
				continue
			}

			for _, keyDir := range keyDirs {
				if !keyDir.IsDir() {
					continue
				}
				cacheKey := keyDir.Name()
				keyPath := filepath.Join(artifactPath, cacheKey)

				size, err := cm.calculateDirSize(keyPath)
				if err != nil {
					continue
				}

				entries = append(entries, CacheSizeEntry{
					ProjectID: projectID,
					Artifact:  artifact,
					CacheKey:  cacheKey,
					Size:      size,
				})
			}
		}
	}

	return entries, nil
}

func (cm *CacheManager) calculateDirSize(path string) (int64, error) {
	var size int64
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		size += info.Size()
		return nil
	})
	return size, err
}

func (cm *CacheManager) RemoveCacheEntry(projectID, artifact, cacheKey string) error {
	path := filepath.Join(cm.LocalCacheDir, projectID, artifact, cacheKey)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("failed to remove cache entry: %w", err)
	}

	cm.cleanEmptyParentDirs(filepath.Join(cm.LocalCacheDir, projectID, artifact))
	cm.cleanEmptyParentDirs(filepath.Join(cm.LocalCacheDir, projectID))

	return nil
}

func (cm *CacheManager) cleanEmptyParentDirs(path string) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}
	if len(entries) == 0 {
		os.Remove(path)
	}
}

func (cm *CacheManager) RemoveAllCache() (int, int64, error) {
	entries, err := cm.GetCacheSizes()
	if err != nil {
		return 0, 0, err
	}

	var totalSize int64
	for _, entry := range entries {
		totalSize += entry.Size
	}

	if err := os.RemoveAll(cm.LocalCacheDir); err != nil {
		return 0, 0, fmt.Errorf("failed to remove cache directory: %w", err)
	}

	return len(entries), totalSize, nil
}
