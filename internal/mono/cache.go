package mono

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

type CacheManager struct {
	HomeDir          string
	GlobalCacheDir   string
	LocalCacheDir    string
	SccacheDir       string
	SccacheAvailable bool
}

func NewCacheManager() (*CacheManager, error) {
	homeDir, err := GetMonoHome()
	if err != nil {
		return nil, err
	}

	globalCacheDir := filepath.Join(homeDir, "cache_global")
	localCacheDir := filepath.Join(homeDir, "cache_local")

	cm := &CacheManager{
		HomeDir:        homeDir,
		GlobalCacheDir: globalCacheDir,
		LocalCacheDir:  localCacheDir,
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
	if cm.SccacheAvailable {
		if err := os.MkdirAll(cm.SccacheDir, 0755); err != nil {
			return err
		}
	}
	return nil
}

func (cm *CacheManager) EnvVars(cfg BuildConfig) []string {
	var vars []string

	if cm.shouldEnableSccache(cfg) {
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

		return copyFile(path, dstPath)
	})
}

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
		return nil
	}

	return HardlinkTree(sourcePath, targetInCache)
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
