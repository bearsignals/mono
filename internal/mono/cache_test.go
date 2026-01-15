package mono

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestComputeProjectID(t *testing.T) {
	id1 := ComputeProjectID("/Users/x/project1")
	id2 := ComputeProjectID("/Users/x/project1")
	id3 := ComputeProjectID("/Users/x/project2")

	if id1 != id2 {
		t.Errorf("same path should produce same ID: got %s and %s", id1, id2)
	}

	if id1 == id3 {
		t.Errorf("different paths should produce different IDs: both got %s", id1)
	}

	if len(id1) != 12 {
		t.Errorf("project ID should be 12 chars, got %d: %s", len(id1), id1)
	}
}

func TestComputeCacheKey(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	lockfile := filepath.Join(testDir, "Cargo.lock")
	if err := os.WriteFile(lockfile, []byte("test lockfile content"), 0644); err != nil {
		t.Fatalf("failed to write lockfile: %v", err)
	}

	artifact := ArtifactConfig{
		Name:        "cargo",
		KeyFiles:    []string{"Cargo.lock"},
		KeyCommands: []string{"echo v1.0"},
		Paths:       []string{"target"},
	}

	key1, err := cm.ComputeCacheKey(artifact, testDir)
	if err != nil {
		t.Fatalf("failed to compute cache key: %v", err)
	}

	key2, err := cm.ComputeCacheKey(artifact, testDir)
	if err != nil {
		t.Fatalf("failed to compute cache key: %v", err)
	}

	if key1 != key2 {
		t.Errorf("same inputs should produce same key: got %s and %s", key1, key2)
	}

	if len(key1) != 16 {
		t.Errorf("cache key should be 16 chars, got %d: %s", len(key1), key1)
	}

	if err := os.WriteFile(lockfile, []byte("different content"), 0644); err != nil {
		t.Fatalf("failed to write lockfile: %v", err)
	}

	key3, err := cm.ComputeCacheKey(artifact, testDir)
	if err != nil {
		t.Fatalf("failed to compute cache key: %v", err)
	}

	if key1 == key3 {
		t.Errorf("different lockfile should produce different key: both got %s", key1)
	}
}

func TestComputeCacheKeyMissingKeyFile(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()

	artifact := ArtifactConfig{
		Name:        "cargo",
		KeyFiles:    []string{"Cargo.lock"},
		KeyCommands: []string{"echo v1.0"},
		Paths:       []string{"target"},
	}

	key, err := cm.ComputeCacheKey(artifact, testDir)
	if err != nil {
		t.Fatalf("missing key file should not error: %v", err)
	}

	if key == "" {
		t.Error("should still produce a key from key commands")
	}
}

func TestHardlinkTree(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")

	if err := os.WriteFile(filepath.Join(src, "file.txt"), []byte("content"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(src, "subdir"), 0755); err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "subdir", "nested.txt"), []byte("nested"), 0644); err != nil {
		t.Fatalf("failed to write nested file: %v", err)
	}

	if err := HardlinkTree(src, dst); err != nil {
		t.Fatalf("HardlinkTree failed: %v", err)
	}

	srcInfo, err := os.Stat(filepath.Join(src, "file.txt"))
	if err != nil {
		t.Fatalf("failed to stat src file: %v", err)
	}
	dstInfo, err := os.Stat(filepath.Join(dst, "file.txt"))
	if err != nil {
		t.Fatalf("failed to stat dst file: %v", err)
	}

	srcSys := srcInfo.Sys().(*syscall.Stat_t)
	dstSys := dstInfo.Sys().(*syscall.Stat_t)

	if srcSys.Ino != dstSys.Ino {
		t.Errorf("files should share inode (hardlink): src=%d, dst=%d", srcSys.Ino, dstSys.Ino)
	}

	nestedDst := filepath.Join(dst, "subdir", "nested.txt")
	if _, err := os.Stat(nestedDst); err != nil {
		t.Errorf("nested file should exist at %s: %v", nestedDst, err)
	}
}

func TestHardlinkTreeReplaceBreaksLink(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "dst")

	srcFile := filepath.Join(src, "file.txt")
	if err := os.WriteFile(srcFile, []byte("original"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	if err := HardlinkTree(src, dst); err != nil {
		t.Fatalf("HardlinkTree failed: %v", err)
	}

	dstFile := filepath.Join(dst, "file.txt")

	srcInfoBefore, _ := os.Stat(srcFile)
	dstInfoBefore, _ := os.Stat(dstFile)
	srcInodeBefore := srcInfoBefore.Sys().(*syscall.Stat_t).Ino
	dstInodeBefore := dstInfoBefore.Sys().(*syscall.Stat_t).Ino

	if srcInodeBefore != dstInodeBefore {
		t.Fatalf("inodes should match before modification")
	}

	if err := os.Remove(dstFile); err != nil {
		t.Fatalf("failed to remove dst file: %v", err)
	}
	if err := os.WriteFile(dstFile, []byte("modified"), 0644); err != nil {
		t.Fatalf("failed to write new dst file: %v", err)
	}

	srcInfoAfter, _ := os.Stat(srcFile)
	dstInfoAfter, _ := os.Stat(dstFile)
	srcInodeAfter := srcInfoAfter.Sys().(*syscall.Stat_t).Ino
	dstInodeAfter := dstInfoAfter.Sys().(*syscall.Stat_t).Ino

	if srcInodeAfter != srcInodeBefore {
		t.Error("src inode should be unchanged")
	}

	if dstInodeAfter == srcInodeAfter {
		t.Error("after replace, dst should have different inode")
	}

	srcContent, _ := os.ReadFile(srcFile)
	if string(srcContent) != "original" {
		t.Errorf("src file should be unchanged, got: %s", srcContent)
	}

	dstContent, _ := os.ReadFile(dstFile)
	if string(dstContent) != "modified" {
		t.Errorf("dst file should be modified, got: %s", dstContent)
	}
}

func TestStoreAndRestoreCache(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	envPath := filepath.Join(testDir, "env")
	targetDir := filepath.Join(envPath, "target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	testFile := filepath.Join(targetDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("cached content"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	cacheDir := filepath.Join(testDir, "cache")
	entry := ArtifactCacheEntry{
		Name:      "cargo",
		Key:       "testkey123",
		CachePath: filepath.Join(cacheDir, "cargo", "testkey123"),
		EnvPaths:  []string{targetDir},
		Hit:       false,
	}

	if err := cm.StoreToCache(entry); err != nil {
		t.Fatalf("StoreToCache failed: %v", err)
	}

	cachedFile := filepath.Join(entry.CachePath, "target", "test.txt")
	if _, err := os.Stat(cachedFile); err != nil {
		t.Errorf("cached file should exist: %v", err)
	}

	if _, err := os.Stat(testFile); err != nil {
		t.Errorf("env file should still exist (hardlinked back): %v", err)
	}

	if err := os.RemoveAll(targetDir); err != nil {
		t.Fatalf("failed to remove target dir: %v", err)
	}

	entry.Hit = true
	if err := cm.RestoreFromCache(entry, nil); err != nil {
		t.Fatalf("RestoreFromCache failed: %v", err)
	}

	restoredContent, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("failed to read restored file: %v", err)
	}

	if string(restoredContent) != "cached content" {
		t.Errorf("restored content mismatch: got %s", restoredContent)
	}
}

func TestDetectArtifacts(t *testing.T) {
	testDir := t.TempDir()

	artifacts := detectArtifacts(testDir)
	if len(artifacts) != 0 {
		t.Errorf("should detect no artifacts in empty dir, got %d", len(artifacts))
	}

	if err := os.WriteFile(filepath.Join(testDir, "Cargo.lock"), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write Cargo.lock: %v", err)
	}

	artifacts = detectArtifacts(testDir)
	if len(artifacts) != 1 {
		t.Errorf("should detect 1 artifact, got %d", len(artifacts))
	}
	if artifacts[0].Name != "cargo" {
		t.Errorf("should detect cargo, got %s", artifacts[0].Name)
	}

	if err := os.WriteFile(filepath.Join(testDir, "package-lock.json"), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write package-lock.json: %v", err)
	}

	artifacts = detectArtifacts(testDir)
	if len(artifacts) != 2 {
		t.Errorf("should detect 2 artifacts, got %d", len(artifacts))
	}
}

func TestDetectNestedArtifacts(t *testing.T) {
	testDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(testDir, "web"), 0755); err != nil {
		t.Fatalf("failed to create web dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "web", "package-lock.json"), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write web/package-lock.json: %v", err)
	}

	artifacts := detectArtifacts(testDir)
	if len(artifacts) != 1 {
		t.Fatalf("should detect 1 artifact, got %d", len(artifacts))
	}

	a := artifacts[0]
	if a.Name != "npm-web" {
		t.Errorf("expected name 'npm-web', got %s", a.Name)
	}
	if len(a.KeyFiles) != 1 || a.KeyFiles[0] != "web/package-lock.json" {
		t.Errorf("expected key_files ['web/package-lock.json'], got %v", a.KeyFiles)
	}
	if len(a.Paths) != 1 || a.Paths[0] != "web/node_modules" {
		t.Errorf("expected paths ['web/node_modules'], got %v", a.Paths)
	}
}

func TestDetectMixedArtifacts(t *testing.T) {
	testDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(testDir, "Cargo.lock"), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write Cargo.lock: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(testDir, "web"), 0755); err != nil {
		t.Fatalf("failed to create web dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "web", "package-lock.json"), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write web/package-lock.json: %v", err)
	}

	artifacts := detectArtifacts(testDir)
	if len(artifacts) != 2 {
		t.Fatalf("should detect 2 artifacts, got %d", len(artifacts))
	}

	names := make(map[string]bool)
	for _, a := range artifacts {
		names[a.Name] = true
	}

	if !names["cargo"] {
		t.Error("should detect cargo artifact")
	}
	if !names["npm-web"] {
		t.Error("should detect npm-web artifact")
	}
}

func TestDetectSkipsNodeModules(t *testing.T) {
	testDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(testDir, "node_modules", "some-pkg"), 0755); err != nil {
		t.Fatalf("failed to create node_modules dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "node_modules", "some-pkg", "package-lock.json"), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write package-lock.json in node_modules: %v", err)
	}

	artifacts := detectArtifacts(testDir)
	if len(artifacts) != 0 {
		t.Errorf("should not detect artifacts inside node_modules, got %d", len(artifacts))
	}
}

func TestDetectDeeplyNestedArtifacts(t *testing.T) {
	testDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(testDir, "packages", "frontend"), 0755); err != nil {
		t.Fatalf("failed to create packages/frontend dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(testDir, "packages", "frontend", "yarn.lock"), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write yarn.lock: %v", err)
	}

	artifacts := detectArtifacts(testDir)
	if len(artifacts) != 1 {
		t.Fatalf("should detect 1 artifact, got %d", len(artifacts))
	}

	a := artifacts[0]
	if a.Name != "yarn-packages-frontend" {
		t.Errorf("expected name 'yarn-packages-frontend', got %s", a.Name)
	}
	if a.Paths[0] != filepath.Join("packages", "frontend", "node_modules") {
		t.Errorf("expected path 'packages/frontend/node_modules', got %s", a.Paths[0])
	}
}

func TestCleanNodeModulesBin(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	nodeModules := t.TempDir()
	binDir := filepath.Join(nodeModules, ".bin")

	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("failed to create .bin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "cli"), []byte("#!/bin/bash"), 0755); err != nil {
		t.Fatalf("failed to write cli file: %v", err)
	}

	if err := cm.cleanNodeModulesBin(nodeModules); err != nil {
		t.Fatalf("cleanNodeModulesBin failed: %v", err)
	}

	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Error(".bin directory should be removed")
	}
}

func TestPrepareArtifactCache(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	envPath := filepath.Join(testDir, "env")
	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(envPath, "Cargo.lock"), []byte("lockfile"), 0644); err != nil {
		t.Fatalf("failed to write Cargo.lock: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	entries, err := cm.PrepareArtifactCache(artifacts, testDir, envPath)
	if err != nil {
		t.Fatalf("PrepareArtifactCache failed: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	entry := entries[0]
	if entry.Name != "cargo" {
		t.Errorf("expected name 'cargo', got %s", entry.Name)
	}
	if entry.Key == "" {
		t.Error("key should not be empty")
	}
	if entry.Hit {
		t.Error("should be cache miss (cache doesn't exist)")
	}
	if len(entry.EnvPaths) != 1 {
		t.Errorf("expected 1 env path, got %d", len(entry.EnvPaths))
	}
}

func TestIntegrationCacheHitMiss(t *testing.T) {
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not installed, skipping integration test")
	}

	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()

	cmd := exec.Command("cargo", "new", "testproj", "--quiet")
	cmd.Dir = testDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create cargo project: %v", err)
	}

	envPath := filepath.Join(testDir, "testproj")

	cmd = exec.Command("cargo", "generate-lockfile", "--quiet")
	cmd.Dir = envPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to generate lockfile: %v", err)
	}

	cmd = exec.Command("cargo", "build", "--quiet")
	cmd.Dir = envPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to build project: %v", err)
	}

	artifacts := detectArtifacts(envPath)
	if len(artifacts) != 1 || artifacts[0].Name != "cargo" {
		t.Fatalf("expected cargo artifact, got %v", artifacts)
	}

	entries, err := cm.PrepareArtifactCache(artifacts, testDir, envPath)
	if err != nil {
		t.Fatalf("PrepareArtifactCache failed: %v", err)
	}

	if entries[0].Hit {
		t.Error("first run should be cache miss")
	}

	if err := cm.StoreToCache(entries[0]); err != nil {
		t.Fatalf("StoreToCache failed: %v", err)
	}

	entries2, err := cm.PrepareArtifactCache(artifacts, testDir, envPath)
	if err != nil {
		t.Fatalf("PrepareArtifactCache failed: %v", err)
	}

	if !entries2[0].Hit {
		t.Error("second run should be cache hit")
	}

	if entries[0].Key != entries2[0].Key {
		t.Errorf("cache keys should match: %s vs %s", entries[0].Key, entries2[0].Key)
	}

	targetDir := filepath.Join(envPath, "target")
	if err := os.RemoveAll(targetDir); err != nil {
		t.Fatalf("failed to remove target: %v", err)
	}

	if err := cm.RestoreFromCache(entries2[0], nil); err != nil {
		t.Fatalf("RestoreFromCache failed: %v", err)
	}

	if _, err := os.Stat(targetDir); err != nil {
		t.Error("target should be restored")
	}

	fpDir := filepath.Join(targetDir, "debug", ".fingerprint")
	if _, err := os.Stat(fpDir); err != nil {
		t.Error("fingerprints should be preserved after restore")
	}
}

func TestSync(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	envPath := filepath.Join(testDir, "env")

	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(envPath, "Cargo.lock"), []byte("lockfile content"), 0644); err != nil {
		t.Fatalf("failed to write Cargo.lock: %v", err)
	}

	targetDir := filepath.Join(envPath, "target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "artifact.txt"), []byte("artifact"), 0644); err != nil {
		t.Fatalf("failed to write artifact: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	err = cm.Sync(artifacts, rootPath, envPath, SyncOptions{HardlinkBack: true})
	if err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	key, _ := cm.ComputeCacheKey(artifacts[0], envPath)
	cachePath := cm.GetArtifactCachePath(rootPath, "cargo", key)
	cachedFile := filepath.Join(cachePath, "target", "artifact.txt")

	if _, err := os.Stat(cachedFile); err != nil {
		t.Errorf("cached artifact should exist: %v", err)
	}

	if _, err := os.Stat(filepath.Join(targetDir, "artifact.txt")); err != nil {
		t.Errorf("local artifact should still exist (hardlinked back): %v", err)
	}

	srcInfo, _ := os.Stat(cachedFile)
	dstInfo, _ := os.Stat(filepath.Join(targetDir, "artifact.txt"))
	srcIno := srcInfo.Sys().(*syscall.Stat_t).Ino
	dstIno := dstInfo.Sys().(*syscall.Stat_t).Ino

	if srcIno != dstIno {
		t.Error("cached and local files should share inode (hardlink)")
	}
}

func TestSyncAlreadyCached(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	envPath := filepath.Join(testDir, "env")

	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(envPath, "Cargo.lock"), []byte("lockfile"), 0644); err != nil {
		t.Fatalf("failed to write Cargo.lock: %v", err)
	}

	targetDir := filepath.Join(envPath, "target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "artifact.txt"), []byte("original"), 0644); err != nil {
		t.Fatalf("failed to write artifact: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	err = cm.Sync(artifacts, rootPath, envPath, SyncOptions{HardlinkBack: true})
	if err != nil {
		t.Fatalf("first sync failed: %v", err)
	}

	key, _ := cm.ComputeCacheKey(artifacts[0], envPath)
	cachePath := cm.GetArtifactCachePath(rootPath, "cargo", key)
	cachedFile := filepath.Join(cachePath, "target", "artifact.txt")

	cacheInfoBefore, _ := os.Stat(cachedFile)
	cacheInoBefore := cacheInfoBefore.Sys().(*syscall.Stat_t).Ino

	if err := os.RemoveAll(targetDir); err != nil {
		t.Fatalf("failed to remove target dir: %v", err)
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to recreate target dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "artifact.txt"), []byte("new content"), 0644); err != nil {
		t.Fatalf("failed to write new artifact: %v", err)
	}

	err = cm.Sync(artifacts, rootPath, envPath, SyncOptions{HardlinkBack: true})
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}

	cacheInfoAfter, _ := os.Stat(cachedFile)
	cacheInoAfter := cacheInfoAfter.Sys().(*syscall.Stat_t).Ino

	if cacheInoBefore != cacheInoAfter {
		t.Error("cache inode should not change when sync skips (already cached)")
	}

	cachedContent, _ := os.ReadFile(cachedFile)
	if string(cachedContent) != "original" {
		t.Errorf("cached content should be original (sync should skip), got: %s", cachedContent)
	}
}

func TestSyncBuildInProgress(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	envPath := filepath.Join(testDir, "env")

	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(envPath, "Cargo.lock"), []byte("lockfile"), 0644); err != nil {
		t.Fatalf("failed to write Cargo.lock: %v", err)
	}

	targetDir := filepath.Join(envPath, "target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	cargoLock := filepath.Join(targetDir, ".cargo-lock")
	if err := os.WriteFile(cargoLock, []byte(""), 0644); err != nil {
		t.Fatalf("failed to write .cargo-lock: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	err = cm.Sync(artifacts, rootPath, envPath, SyncOptions{HardlinkBack: true})
	if err == nil {
		t.Error("sync should fail when build is in progress")
	}
}

func TestSyncNoArtifacts(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	envPath := filepath.Join(testDir, "env")

	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(envPath, "Cargo.lock"), []byte("lockfile"), 0644); err != nil {
		t.Fatalf("failed to write Cargo.lock: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	err = cm.Sync(artifacts, rootPath, envPath, SyncOptions{HardlinkBack: true})
	if err != nil {
		t.Errorf("sync should succeed (no-op) when artifacts don't exist: %v", err)
	}
}

func TestSyncMissingLockfile(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	envPath := filepath.Join(testDir, "env")

	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	targetDir := filepath.Join(envPath, "target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	err = cm.Sync(artifacts, rootPath, envPath, SyncOptions{HardlinkBack: true})
	if err != nil {
		t.Errorf("sync should skip silently when lockfile missing: %v", err)
	}
}

func TestIsBuildInProgress(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	targetDir := filepath.Join(testDir, "target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}

	cargoArtifact := ArtifactConfig{Name: "cargo"}
	npmArtifact := ArtifactConfig{Name: "npm"}

	if cm.isBuildInProgress(testDir, cargoArtifact) {
		t.Error("should not detect build in progress without .cargo-lock")
	}

	cargoLock := filepath.Join(targetDir, ".cargo-lock")
	if err := os.WriteFile(cargoLock, []byte(""), 0644); err != nil {
		t.Fatalf("failed to write .cargo-lock: %v", err)
	}

	if !cm.isBuildInProgress(testDir, cargoArtifact) {
		t.Error("should detect cargo build in progress with .cargo-lock")
	}

	if cm.isBuildInProgress(testDir, npmArtifact) {
		t.Error("npm should not detect cargo's build lock")
	}
}

func TestSeedFromRoot(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	envPath := filepath.Join(testDir, "env")

	if err := os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatalf("failed to create root dir: %v", err)
	}
	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	lockfileContent := []byte("lockfile content")
	if err := os.WriteFile(filepath.Join(rootPath, "Cargo.lock"), lockfileContent, 0644); err != nil {
		t.Fatalf("failed to write root Cargo.lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(envPath, "Cargo.lock"), lockfileContent, 0644); err != nil {
		t.Fatalf("failed to write env Cargo.lock: %v", err)
	}

	rootTarget := filepath.Join(rootPath, "target")
	if err := os.MkdirAll(rootTarget, 0755); err != nil {
		t.Fatalf("failed to create root target dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootTarget, "artifact.txt"), []byte("from root"), 0644); err != nil {
		t.Fatalf("failed to write root artifact: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	err = cm.SeedFromRoot(artifacts, rootPath, envPath, nil)
	if err != nil {
		t.Fatalf("SeedFromRoot failed: %v", err)
	}

	key, _ := cm.ComputeCacheKey(artifacts[0], envPath)
	cachePath := cm.GetArtifactCachePath(rootPath, "cargo", key)
	cachedFile := filepath.Join(cachePath, "target", "artifact.txt")

	if _, err := os.Stat(cachedFile); err != nil {
		t.Errorf("cached artifact should exist after seeding: %v", err)
	}

	cachedContent, _ := os.ReadFile(cachedFile)
	if string(cachedContent) != "from root" {
		t.Errorf("cached content should match root, got: %s", cachedContent)
	}

	rootInfo, _ := os.Stat(filepath.Join(rootTarget, "artifact.txt"))
	cacheInfo, _ := os.Stat(cachedFile)
	rootIno := rootInfo.Sys().(*syscall.Stat_t).Ino
	cacheIno := cacheInfo.Sys().(*syscall.Stat_t).Ino

	if rootIno != cacheIno {
		t.Error("root and cache should share inode (hardlink)")
	}
}

func TestSeedSkipsSameDirectory(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(testDir, "Cargo.lock"), []byte("lockfile"), 0644); err != nil {
		t.Fatalf("failed to write Cargo.lock: %v", err)
	}

	targetDir := filepath.Join(testDir, "target")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "artifact.txt"), []byte("content"), 0644); err != nil {
		t.Fatalf("failed to write artifact: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	err = cm.SeedFromRoot(artifacts, testDir, testDir, nil)
	if err != nil {
		t.Fatalf("SeedFromRoot failed: %v", err)
	}

	key, _ := cm.ComputeCacheKey(artifacts[0], testDir)
	cachePath := cm.GetArtifactCachePath(testDir, "cargo", key)

	if dirExists(cachePath) {
		t.Error("cache should not be created when rootPath == envPath")
	}
}

func TestSeedSkipsDifferentLockfiles(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	envPath := filepath.Join(testDir, "env")

	if err := os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatalf("failed to create root dir: %v", err)
	}
	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(rootPath, "Cargo.lock"), []byte("root lockfile"), 0644); err != nil {
		t.Fatalf("failed to write root Cargo.lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(envPath, "Cargo.lock"), []byte("env lockfile"), 0644); err != nil {
		t.Fatalf("failed to write env Cargo.lock: %v", err)
	}

	rootTarget := filepath.Join(rootPath, "target")
	if err := os.MkdirAll(rootTarget, 0755); err != nil {
		t.Fatalf("failed to create root target dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootTarget, "artifact.txt"), []byte("from root"), 0644); err != nil {
		t.Fatalf("failed to write root artifact: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	err = cm.SeedFromRoot(artifacts, rootPath, envPath, nil)
	if err != nil {
		t.Fatalf("SeedFromRoot failed: %v", err)
	}

	key, _ := cm.ComputeCacheKey(artifacts[0], envPath)
	cachePath := cm.GetArtifactCachePath(rootPath, "cargo", key)

	if dirExists(cachePath) {
		t.Error("cache should not be created when lockfiles differ")
	}
}

func TestSeedSkipsNoRootArtifacts(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	envPath := filepath.Join(testDir, "env")

	if err := os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatalf("failed to create root dir: %v", err)
	}
	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	lockfileContent := []byte("lockfile content")
	if err := os.WriteFile(filepath.Join(rootPath, "Cargo.lock"), lockfileContent, 0644); err != nil {
		t.Fatalf("failed to write root Cargo.lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(envPath, "Cargo.lock"), lockfileContent, 0644); err != nil {
		t.Fatalf("failed to write env Cargo.lock: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	err = cm.SeedFromRoot(artifacts, rootPath, envPath, nil)
	if err != nil {
		t.Fatalf("SeedFromRoot failed: %v", err)
	}

	key, _ := cm.ComputeCacheKey(artifacts[0], envPath)
	cachePath := cm.GetArtifactCachePath(rootPath, "cargo", key)

	if dirExists(cachePath) {
		t.Error("cache should not be created when root has no artifacts")
	}
}

func TestSeedSkipsBuildInProgress(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	envPath := filepath.Join(testDir, "env")

	if err := os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatalf("failed to create root dir: %v", err)
	}
	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	lockfileContent := []byte("lockfile content")
	if err := os.WriteFile(filepath.Join(rootPath, "Cargo.lock"), lockfileContent, 0644); err != nil {
		t.Fatalf("failed to write root Cargo.lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(envPath, "Cargo.lock"), lockfileContent, 0644); err != nil {
		t.Fatalf("failed to write env Cargo.lock: %v", err)
	}

	rootTarget := filepath.Join(rootPath, "target")
	if err := os.MkdirAll(rootTarget, 0755); err != nil {
		t.Fatalf("failed to create root target dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootTarget, "artifact.txt"), []byte("from root"), 0644); err != nil {
		t.Fatalf("failed to write root artifact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootTarget, ".cargo-lock"), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write .cargo-lock: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	err = cm.SeedFromRoot(artifacts, rootPath, envPath, nil)
	if err != nil {
		t.Fatalf("SeedFromRoot failed: %v", err)
	}

	key, _ := cm.ComputeCacheKey(artifacts[0], envPath)
	cachePath := cm.GetArtifactCachePath(rootPath, "cargo", key)

	if dirExists(cachePath) {
		t.Error("cache should not be created when root build is in progress")
	}
}

func TestSeedAlreadyCached(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	envPath := filepath.Join(testDir, "env")

	if err := os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatalf("failed to create root dir: %v", err)
	}
	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	lockfileContent := []byte("lockfile content")
	if err := os.WriteFile(filepath.Join(rootPath, "Cargo.lock"), lockfileContent, 0644); err != nil {
		t.Fatalf("failed to write root Cargo.lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(envPath, "Cargo.lock"), lockfileContent, 0644); err != nil {
		t.Fatalf("failed to write env Cargo.lock: %v", err)
	}

	rootTarget := filepath.Join(rootPath, "target")
	if err := os.MkdirAll(rootTarget, 0755); err != nil {
		t.Fatalf("failed to create root target dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootTarget, "artifact.txt"), []byte("from root"), 0644); err != nil {
		t.Fatalf("failed to write root artifact: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	key, _ := cm.ComputeCacheKey(artifacts[0], envPath)
	cachePath := cm.GetArtifactCachePath(rootPath, "cargo", key)
	cacheTarget := filepath.Join(cachePath, "target")
	if err := os.MkdirAll(cacheTarget, 0755); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheTarget, "artifact.txt"), []byte("existing cache"), 0644); err != nil {
		t.Fatalf("failed to write existing cache: %v", err)
	}

	err = cm.SeedFromRoot(artifacts, rootPath, envPath, nil)
	if err != nil {
		t.Fatalf("SeedFromRoot failed: %v", err)
	}

	cachedContent, _ := os.ReadFile(filepath.Join(cacheTarget, "artifact.txt"))
	if string(cachedContent) != "existing cache" {
		t.Errorf("existing cache should not be overwritten, got: %s", cachedContent)
	}
}

func TestSeedThenRestore(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	envPath := filepath.Join(testDir, "env")

	if err := os.MkdirAll(rootPath, 0755); err != nil {
		t.Fatalf("failed to create root dir: %v", err)
	}
	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create env dir: %v", err)
	}

	lockfileContent := []byte("lockfile content")
	if err := os.WriteFile(filepath.Join(rootPath, "Cargo.lock"), lockfileContent, 0644); err != nil {
		t.Fatalf("failed to write root Cargo.lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(envPath, "Cargo.lock"), lockfileContent, 0644); err != nil {
		t.Fatalf("failed to write env Cargo.lock: %v", err)
	}

	rootTarget := filepath.Join(rootPath, "target")
	if err := os.MkdirAll(filepath.Join(rootTarget, "debug", ".fingerprint"), 0755); err != nil {
		t.Fatalf("failed to create root fingerprint dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootTarget, "artifact.txt"), []byte("from root"), 0644); err != nil {
		t.Fatalf("failed to write root artifact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootTarget, "debug", ".fingerprint", "fp.txt"), []byte("fingerprint"), 0644); err != nil {
		t.Fatalf("failed to write fingerprint: %v", err)
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	err = cm.SeedFromRoot(artifacts, rootPath, envPath, nil)
	if err != nil {
		t.Fatalf("SeedFromRoot failed: %v", err)
	}

	entries, err := cm.PrepareArtifactCache(artifacts, rootPath, envPath)
	if err != nil {
		t.Fatalf("PrepareArtifactCache failed: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if !entries[0].Hit {
		t.Error("should be cache hit after seeding")
	}

	err = cm.RestoreFromCache(entries[0], nil)
	if err != nil {
		t.Fatalf("RestoreFromCache failed: %v", err)
	}

	envTarget := filepath.Join(envPath, "target")
	restoredContent, err := os.ReadFile(filepath.Join(envTarget, "artifact.txt"))
	if err != nil {
		t.Fatalf("failed to read restored artifact: %v", err)
	}

	if string(restoredContent) != "from root" {
		t.Errorf("restored content should match root, got: %s", restoredContent)
	}

	fpDir := filepath.Join(envTarget, "debug", ".fingerprint")
	if _, err := os.Stat(fpDir); err != nil {
		t.Error("fingerprints should be preserved after restore")
	}
}

func TestConcurrentSync(t *testing.T) {
	cm, err := NewCacheManager()
	if err != nil {
		t.Fatalf("failed to create cache manager: %v", err)
	}

	testDir := t.TempDir()
	rootPath := filepath.Join(testDir, "root")
	env1Path := filepath.Join(testDir, "env1")
	env2Path := filepath.Join(testDir, "env2")

	for _, p := range []string{env1Path, env2Path} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatalf("failed to create env dir: %v", err)
		}

		if err := os.WriteFile(filepath.Join(p, "Cargo.lock"), []byte("same lockfile"), 0644); err != nil {
			t.Fatalf("failed to write Cargo.lock: %v", err)
		}

		targetDir := filepath.Join(p, "target")
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			t.Fatalf("failed to create target dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(targetDir, "artifact.txt"), []byte("content from "+p), 0644); err != nil {
			t.Fatalf("failed to write artifact: %v", err)
		}
	}

	artifacts := []ArtifactConfig{
		{
			Name:        "cargo",
			KeyFiles:    []string{"Cargo.lock"},
			KeyCommands: []string{"echo v1"},
			Paths:       []string{"target"},
		},
	}

	done := make(chan error, 2)

	go func() {
		done <- cm.Sync(artifacts, rootPath, env1Path, SyncOptions{HardlinkBack: true})
	}()

	go func() {
		done <- cm.Sync(artifacts, rootPath, env2Path, SyncOptions{HardlinkBack: true})
	}()

	err1 := <-done
	err2 := <-done

	if err1 != nil && err2 != nil {
		t.Errorf("at least one sync should succeed: err1=%v, err2=%v", err1, err2)
	}

	key, _ := cm.ComputeCacheKey(artifacts[0], env1Path)
	cachePath := cm.GetArtifactCachePath(rootPath, "cargo", key)
	cachedFile := filepath.Join(cachePath, "target", "artifact.txt")

	if _, err := os.Stat(cachedFile); err != nil {
		t.Errorf("cache entry should exist: %v", err)
	}
}

func TestShouldSkipCargoPath(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"foo.o", true},
		{"debug/deps/foo.o", true},
		{"foo.d", true},
		{"debug/deps/foo.d", true},
		{"incremental/foo/bar.bin", true},
		{"debug/incremental/foo/bar", true},
		{".cargo-lock", true},
		{"foo.rlib", false},
		{"foo.rmeta", false},
		{"build/foo/output", false},
		{"debug/deps/libfoo.rlib", false},
		{"release/deps/libfoo.a", false},
		{"deps/foo.dylib", false},
	}

	for _, tt := range tests {
		result := shouldSkipCargoPath(tt.path)
		if result != tt.expected {
			t.Errorf("shouldSkipCargoPath(%q) = %v, want %v", tt.path, result, tt.expected)
		}
	}
}

func TestShouldSkipPath(t *testing.T) {
	tests := []struct {
		path         string
		artifactName string
		expected     bool
	}{
		{"foo.o", "cargo", true},
		{"foo.d", "cargo", true},
		{"incremental/foo", "cargo", true},
		{"foo.rlib", "cargo", false},
		{"foo.o", "npm", false},
		{"foo.d", "npm", false},
		{"node_modules/foo", "npm", false},
		{"foo.o", "", false},
	}

	for _, tt := range tests {
		result := shouldSkipPath(tt.path, tt.artifactName)
		if result != tt.expected {
			t.Errorf("shouldSkipPath(%q, %q) = %v, want %v", tt.path, tt.artifactName, result, tt.expected)
		}
	}
}

func TestSeedDirectorySkipsCargoFiles(t *testing.T) {
	testDir := t.TempDir()
	srcDir := filepath.Join(testDir, "src")
	dstDir := filepath.Join(testDir, "dst")

	if err := os.MkdirAll(filepath.Join(srcDir, "debug", "deps"), 0755); err != nil {
		t.Fatalf("failed to create source dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "debug", "incremental", "foo"), 0755); err != nil {
		t.Fatalf("failed to create incremental dir: %v", err)
	}

	files := map[string]string{
		"debug/deps/libfoo.rlib":          "rlib content",
		"debug/deps/libfoo.rmeta":         "rmeta content",
		"debug/deps/foo.o":                "object file",
		"debug/deps/foo.d":                "dep file",
		"debug/incremental/foo/cache.bin": "incremental cache",
		".cargo-lock":                     "lock",
	}

	for path, content := range files {
		fullPath := filepath.Join(srcDir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("failed to create dir for %s: %v", path, err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", path, err)
		}
	}

	err := SeedDirectory(srcDir, dstDir, SeedOptions{
		ArtifactName: "cargo",
	})
	if err != nil {
		t.Fatalf("SeedDirectory failed: %v", err)
	}

	shouldExist := []string{
		"debug/deps/libfoo.rlib",
		"debug/deps/libfoo.rmeta",
	}
	shouldNotExist := []string{
		"debug/deps/foo.o",
		"debug/deps/foo.d",
		"debug/incremental/foo/cache.bin",
		".cargo-lock",
	}

	for _, path := range shouldExist {
		fullPath := filepath.Join(dstDir, path)
		if _, err := os.Stat(fullPath); err != nil {
			t.Errorf("expected %s to exist in destination: %v", path, err)
		}
	}

	for _, path := range shouldNotExist {
		fullPath := filepath.Join(dstDir, path)
		if _, err := os.Stat(fullPath); !os.IsNotExist(err) {
			t.Errorf("expected %s to NOT exist in destination", path)
		}
	}
}

func TestSeedDirectoryNoSkipForOtherArtifacts(t *testing.T) {
	testDir := t.TempDir()
	srcDir := filepath.Join(testDir, "src")
	dstDir := filepath.Join(testDir, "dst")

	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatalf("failed to create source dir: %v", err)
	}

	files := []string{"foo.o", "bar.d", "baz.rlib"}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(srcDir, f), []byte("content"), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", f, err)
		}
	}

	err := SeedDirectory(srcDir, dstDir, SeedOptions{
		ArtifactName: "npm",
	})
	if err != nil {
		t.Fatalf("SeedDirectory failed: %v", err)
	}

	for _, f := range files {
		fullPath := filepath.Join(dstDir, f)
		if _, err := os.Stat(fullPath); err != nil {
			t.Errorf("expected %s to exist in destination: %v", f, err)
		}
	}
}

func TestCountFiles(t *testing.T) {
	testDir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(testDir, "debug", "deps"), 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(testDir, "debug", "incremental"), 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	files := []string{
		"debug/deps/libfoo.rlib",
		"debug/deps/libfoo.rmeta",
		"debug/deps/foo.o",
		"debug/deps/foo.d",
		"debug/incremental/cache.bin",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(testDir, f), []byte("content"), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", f, err)
		}
	}

	count, err := countFiles(testDir, "cargo")
	if err != nil {
		t.Fatalf("countFiles failed: %v", err)
	}

	if count != 2 {
		t.Errorf("expected 2 files (rlib, rmeta), got %d", count)
	}

	countAll, err := countFiles(testDir, "")
	if err != nil {
		t.Fatalf("countFiles failed: %v", err)
	}

	if countAll != 5 {
		t.Errorf("expected 5 files with no skip, got %d", countAll)
	}
}

func setupMockFingerprints(b *testing.B, numCrates int) string {
	b.Helper()
	dir := b.TempDir()
	fpDir := filepath.Join(dir, "debug", ".fingerprint")

	for i := 0; i < numCrates; i++ {
		crateDir := filepath.Join(fpDir, fmt.Sprintf("crate-%d-abc123", i))
		if err := os.MkdirAll(crateDir, 0755); err != nil {
			b.Fatalf("failed to create crate dir: %v", err)
		}
		files := []string{
			"dep-lib-crate",
			"invoked.timestamp",
			"lib-crate.json",
			"abc123def456",
		}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(crateDir, f), []byte("content"), 0644); err != nil {
				b.Fatalf("failed to write file: %v", err)
			}
		}
	}

	return dir
}

func touchDepFilesFind(fingerprintDir string) error {
	cmd := exec.Command("find", fingerprintDir, "-type", "f", "-name", "dep-*", "-exec", "touch", "{}", "+")
	return cmd.Run()
}

func BenchmarkTouchDepFiles(b *testing.B) {
	for _, numCrates := range []int{100, 500} {
		b.Run(fmt.Sprintf("sequential/crates=%d", numCrates), func(b *testing.B) {
			targetDir := setupMockFingerprints(b, numCrates)
			fpDir := filepath.Join(targetDir, "debug", ".fingerprint")

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := touchDepFiles(fpDir, time.Now()); err != nil {
					b.Fatalf("touchDepFiles failed: %v", err)
				}
			}
		})

		b.Run(fmt.Sprintf("parallel/crates=%d", numCrates), func(b *testing.B) {
			targetDir := setupMockFingerprints(b, numCrates)
			fpDir := filepath.Join(targetDir, "debug", ".fingerprint")

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := touchDepFilesParallel(fpDir, time.Now(), 8); err != nil {
					b.Fatalf("touchDepFilesParallel failed: %v", err)
				}
			}
		})

		b.Run(fmt.Sprintf("find/crates=%d", numCrates), func(b *testing.B) {
			targetDir := setupMockFingerprints(b, numCrates)
			fpDir := filepath.Join(targetDir, "debug", ".fingerprint")

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := touchDepFilesFind(fpDir); err != nil {
					b.Fatalf("touchDepFilesFind failed: %v", err)
				}
			}
		})
	}
}
