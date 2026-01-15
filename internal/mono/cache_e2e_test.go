package mono

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestE2ECacheFlow(t *testing.T) {
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not installed, skipping e2e test")
	}

	projectRoot := findProjectRoot(t)
	monoBin := filepath.Join(projectRoot, "mono")

	t.Log("Building mono binary...")
	cmd := exec.Command("go", "build", "-o", monoBin, "./cmd/mono")
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build mono: %v\n%s", err, out)
	}
	defer os.Remove(monoBin)

	testDir, _ := filepath.EvalSymlinks(t.TempDir())
	envPath := filepath.Join(testDir, "testproj")

	t.Cleanup(func() {
		cleanupTestEnvironment(t, monoBin, envPath, testDir)
	})

	t.Log("Creating test cargo project...")
	cmd = exec.Command("cargo", "new", "testproj", "--quiet")
	cmd.Dir = testDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create cargo project: %v", err)
	}

	cmd = exec.Command("cargo", "generate-lockfile", "--quiet")
	cmd.Dir = envPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to generate lockfile: %v", err)
	}

	monoYml := `scripts:
  init: |
    echo "CACHE_STATUS:MONO_CACHE_HIT=$MONO_CACHE_HIT"
    cargo build --quiet
`
	if err := os.WriteFile(filepath.Join(envPath, "mono.yml"), []byte(monoYml), 0644); err != nil {
		t.Fatalf("failed to write mono.yml: %v", err)
	}

	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".mono", "cache_local")
	os.RemoveAll(cacheDir)

	logFile := filepath.Join(home, ".mono", "mono.log")

	t.Log("Running first mono init (expect cache miss)...")
	cmd = exec.Command(monoBin, "init", ".")
	cmd.Dir = envPath
	cmd.Env = append(os.Environ(), "CONDUCTOR_ROOT_PATH="+testDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mono init failed: %v\n%s", err, out)
	}

	logs, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	logContent := string(logs)

	if !strings.Contains(logContent, "cache miss for cargo") {
		t.Error("expected 'cache miss for cargo' in logs")
	}

	if !strings.Contains(logContent, "stored cargo to cache") {
		t.Error("expected 'stored cargo to cache' in logs")
	}

	if !strings.Contains(logContent, "CACHE_STATUS:MONO_CACHE_HIT=false") {
		t.Error("expected MONO_CACHE_HIT=false on first run")
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatalf("cache directory not created: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected project cache directory to be created")
	}

	projectCacheDir := filepath.Join(cacheDir, entries[0].Name(), "cargo")
	cargoEntries, err := os.ReadDir(projectCacheDir)
	if err != nil {
		t.Fatalf("cargo cache directory not created: %v", err)
	}
	if len(cargoEntries) == 0 {
		t.Fatal("expected cargo cache entry to be created")
	}
	cacheKey := cargoEntries[0].Name()
	t.Logf("Cache key: %s", cacheKey)

	cachedTarget := filepath.Join(projectCacheDir, cacheKey, "target")
	if _, err := os.Stat(cachedTarget); err != nil {
		t.Fatalf("cached target directory not found: %v", err)
	}

	t.Log("Destroying environment...")
	cmd = exec.Command(monoBin, "destroy", ".")
	cmd.Dir = envPath
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("destroy output: %s", out)
	}

	targetDir := filepath.Join(envPath, "target")
	os.RemoveAll(targetDir)

	db, err := OpenDB()
	if err == nil {
		db.DeleteEnvironment(envPath)
		db.Close()
	}

	t.Log("Running second mono init (expect cache hit)...")
	cmd = exec.Command(monoBin, "init", ".")
	cmd.Dir = envPath
	cmd.Env = append(os.Environ(), "CONDUCTOR_ROOT_PATH="+testDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mono init failed: %v\n%s", err, out)
	}

	logs, _ = os.ReadFile(logFile)
	logContent = string(logs)

	expectedHitLog := "cache hit for cargo (key: " + cacheKey + ")"
	if !strings.Contains(logContent, expectedHitLog) {
		t.Errorf("expected '%s' in logs", expectedHitLog)
	}

	if !strings.Contains(logContent, "CACHE_STATUS:MONO_CACHE_HIT=true") {
		t.Error("expected MONO_CACHE_HIT=true on cache hit")
	}

	t.Log("Verifying hardlinks...")
	cachedFiles, _ := filepath.Glob(filepath.Join(cachedTarget, "debug", "deps", "*.rlib"))
	envFiles, _ := filepath.Glob(filepath.Join(targetDir, "debug", "deps", "*.rlib"))

	if len(cachedFiles) > 0 && len(envFiles) > 0 {
		cachedInfo, _ := os.Stat(cachedFiles[0])
		envInfo, _ := os.Stat(envFiles[0])

		cachedInode := cachedInfo.Sys().(*syscall.Stat_t).Ino
		envInode := envInfo.Sys().(*syscall.Stat_t).Ino

		if cachedInode != envInode {
			t.Errorf("hardlinks not working: cached inode %d != env inode %d", cachedInode, envInode)
		} else {
			t.Logf("Hardlinks verified (inode: %d)", cachedInode)
		}
	}

	t.Log("Verifying fingerprints were preserved...")
	fpDir := filepath.Join(targetDir, "debug", ".fingerprint")
	fpEntries, _ := os.ReadDir(fpDir)
	if len(fpEntries) == 0 {
		t.Error("fingerprints should be preserved from cache")
	} else {
		t.Log("Fingerprints preserved from cache (expected)")
	}

	t.Log("Cleaning up...")
	cmd = exec.Command(monoBin, "destroy", ".")
	cmd.Dir = envPath
	cmd.Run()

	t.Log("All e2e cache tests passed!")
}

func TestE2ESccacheDetection(t *testing.T) {
	projectRoot := findProjectRoot(t)
	monoBin := filepath.Join(projectRoot, "mono")

	cmd := exec.Command("go", "build", "-o", monoBin, "./cmd/mono")
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build mono: %v\n%s", err, out)
	}
	defer os.Remove(monoBin)

	testDir, _ := filepath.EvalSymlinks(t.TempDir())
	envPath := filepath.Join(testDir, "testenv")
	if err := os.MkdirAll(envPath, 0755); err != nil {
		t.Fatalf("failed to create test dir: %v", err)
	}

	t.Cleanup(func() {
		cleanupTestEnvironment(t, monoBin, envPath, testDir)
	})

	monoYml := `scripts:
  init: echo "init done"
`
	if err := os.WriteFile(filepath.Join(envPath, "mono.yml"), []byte(monoYml), 0644); err != nil {
		t.Fatalf("failed to write mono.yml: %v", err)
	}

	cmd = exec.Command(monoBin, "init", ".")
	cmd.Dir = envPath
	cmd.Run()

	home, _ := os.UserHomeDir()
	logFile := filepath.Join(home, ".mono", "mono.log")
	logs, _ := os.ReadFile(logFile)
	logContent := string(logs)

	hasSccache := exec.Command("which", "sccache").Run() == nil

	if hasSccache {
		if !strings.Contains(logContent, "sccache detected") {
			t.Error("expected 'sccache detected' when sccache is installed")
		}
	} else {
		if !strings.Contains(logContent, "sccache not found") {
			t.Error("expected 'sccache not found' when sccache is not installed")
		}
		if !strings.Contains(logContent, "hint: install sccache") {
			t.Error("expected sccache install hint")
		}
	}

	cmd = exec.Command(monoBin, "destroy", ".")
	cmd.Dir = envPath
	cmd.Run()
}

func TestE2ECacheKeyChangesWithLockfile(t *testing.T) {
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not installed, skipping e2e test")
	}

	projectRoot := findProjectRoot(t)
	monoBin := filepath.Join(projectRoot, "mono")

	cmd := exec.Command("go", "build", "-o", monoBin, "./cmd/mono")
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build mono: %v\n%s", err, out)
	}
	defer os.Remove(monoBin)

	testDir, _ := filepath.EvalSymlinks(t.TempDir())
	envPath := filepath.Join(testDir, "testproj")

	t.Cleanup(func() {
		cleanupTestEnvironment(t, monoBin, envPath, testDir)
	})

	cmd = exec.Command("cargo", "new", "testproj", "--quiet")
	cmd.Dir = testDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create cargo project: %v", err)
	}

	cmd = exec.Command("cargo", "generate-lockfile", "--quiet")
	cmd.Dir = envPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to generate lockfile: %v", err)
	}

	monoYml := `scripts:
  init: cargo build --quiet
`
	if err := os.WriteFile(filepath.Join(envPath, "mono.yml"), []byte(monoYml), 0644); err != nil {
		t.Fatalf("failed to write mono.yml: %v", err)
	}

	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".mono", "cache_local")
	os.RemoveAll(cacheDir)

	cmd = exec.Command(monoBin, "init", ".")
	cmd.Dir = envPath
	cmd.Env = append(os.Environ(), "CONDUCTOR_ROOT_PATH="+testDir)
	cmd.Run()

	entries, _ := os.ReadDir(cacheDir)
	projectCacheDir := filepath.Join(cacheDir, entries[0].Name(), "cargo")
	cargoEntries1, _ := os.ReadDir(projectCacheDir)
	key1 := cargoEntries1[0].Name()
	t.Logf("First cache key: %s", key1)

	cmd = exec.Command(monoBin, "destroy", ".")
	cmd.Dir = envPath
	cmd.Run()
	os.RemoveAll(filepath.Join(envPath, "target"))

	db, err := OpenDB()
	if err == nil {
		db.DeleteEnvironment(envPath)
		db.Close()
	}

	lockfile := filepath.Join(envPath, "Cargo.lock")
	content, _ := os.ReadFile(lockfile)
	newContent := strings.Replace(string(content), "version = 3", "version = 4", 1)
	if newContent == string(content) {
		newContent = string(content) + "\n[[package]]\nname = \"fake\"\nversion = \"1.0.0\"\n"
	}
	os.WriteFile(lockfile, []byte(newContent), 0644)

	cmd = exec.Command(monoBin, "init", ".")
	cmd.Dir = envPath
	cmd.Env = append(os.Environ(), "CONDUCTOR_ROOT_PATH="+testDir)
	cmd.Run()

	cargoEntries2, _ := os.ReadDir(projectCacheDir)
	var key2 string
	for _, e := range cargoEntries2 {
		if e.Name() != key1 {
			key2 = e.Name()
			break
		}
	}

	if key2 == "" {
		t.Error("expected different cache key after lockfile modification")
	} else {
		t.Logf("Second cache key: %s (different from first)", key2)
	}

	cmd = exec.Command(monoBin, "destroy", ".")
	cmd.Dir = envPath
	cmd.Run()
}

func TestE2ECacheStats(t *testing.T) {
	if _, err := exec.LookPath("cargo"); err != nil {
		t.Skip("cargo not installed, skipping e2e test")
	}

	projectRoot := findProjectRoot(t)
	monoBin := filepath.Join(projectRoot, "mono")

	t.Log("Building mono binary...")
	cmd := exec.Command("go", "build", "-o", monoBin, "./cmd/mono")
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build mono: %v\n%s", err, out)
	}
	defer os.Remove(monoBin)

	testDir, _ := filepath.EvalSymlinks(t.TempDir())
	envPath := filepath.Join(testDir, "testproj")

	t.Cleanup(func() {
		cleanupTestEnvironment(t, monoBin, envPath, testDir)
		db, err := OpenDB()
		if err == nil {
			db.DeleteAllCacheEvents()
			db.Close()
		}
	})

	t.Log("Creating test cargo project...")
	cmd = exec.Command("cargo", "new", "testproj", "--quiet")
	cmd.Dir = testDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create cargo project: %v", err)
	}

	cmd = exec.Command("cargo", "generate-lockfile", "--quiet")
	cmd.Dir = envPath
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to generate lockfile: %v", err)
	}

	monoYml := `scripts:
  init: cargo build --quiet
`
	if err := os.WriteFile(filepath.Join(envPath, "mono.yml"), []byte(monoYml), 0644); err != nil {
		t.Fatalf("failed to write mono.yml: %v", err)
	}

	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".mono", "cache_local")
	os.RemoveAll(cacheDir)

	db, err := OpenDB()
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	db.DeleteAllCacheEvents()
	db.Close()

	t.Log("Running first mono init (cache miss)...")
	cmd = exec.Command(monoBin, "init", ".")
	cmd.Dir = envPath
	cmd.Env = append(os.Environ(), "CONDUCTOR_ROOT_PATH="+testDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mono init failed: %v\n%s", err, out)
	}

	t.Log("Checking cache stats after first init...")
	cmd = exec.Command(monoBin, "cache", "stats")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mono cache stats failed: %v\n%s", err, out)
	}
	statsOutput := string(out)

	if !strings.Contains(statsOutput, "cargo") {
		t.Errorf("expected 'cargo' in stats output, got:\n%s", statsOutput)
	}
	if !strings.Contains(statsOutput, "0") {
		t.Errorf("expected '0' hits in stats output after miss, got:\n%s", statsOutput)
	}

	t.Log("Destroying and recreating environment (cache hit)...")
	cmd = exec.Command(monoBin, "destroy", ".")
	cmd.Dir = envPath
	cmd.Run()

	os.RemoveAll(filepath.Join(envPath, "target"))

	db, err = OpenDB()
	if err == nil {
		db.DeleteEnvironment(envPath)
		db.Close()
	}

	cmd = exec.Command(monoBin, "init", ".")
	cmd.Dir = envPath
	cmd.Env = append(os.Environ(), "CONDUCTOR_ROOT_PATH="+testDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mono init failed: %v\n%s", err, out)
	}

	t.Log("Checking cache stats after cache hit...")
	cmd = exec.Command(monoBin, "cache", "stats")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mono cache stats failed: %v\n%s", err, out)
	}
	statsOutput = string(out)

	if !strings.Contains(statsOutput, "1") {
		t.Errorf("expected '1' hit in stats output after cache hit, got:\n%s", statsOutput)
	}

	t.Log("Testing mono cache clean --all...")
	cmd = exec.Command(monoBin, "cache", "clean", "--all")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mono cache clean --all failed: %v\n%s", err, out)
	}
	cleanOutput := string(out)

	if !strings.Contains(cleanOutput, "Removed") {
		t.Errorf("expected 'Removed' in clean output, got:\n%s", cleanOutput)
	}

	t.Log("Verifying cache is empty after clean...")
	cmd = exec.Command(monoBin, "cache", "stats")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mono cache stats failed: %v\n%s", err, out)
	}
	statsOutput = string(out)

	if !strings.Contains(statsOutput, "No cache entries found") {
		t.Errorf("expected 'No cache entries found' after clean, got:\n%s", statsOutput)
	}

	db, err = OpenDB()
	if err != nil {
		t.Fatalf("failed to open db: %v", err)
	}
	defer db.Close()

	stats, err := db.GetCacheStats()
	if err != nil {
		t.Fatalf("failed to get cache stats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("expected 0 cache events after clean, got %d", len(stats))
	}

	t.Log("Cache stats e2e test passed!")
}

func findProjectRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find project root (no go.mod found)")
		}
		dir = parent
	}
}

func cleanupTestEnvironment(t *testing.T, monoBin, envPath, testDir string) {
	t.Helper()

	cmd := exec.Command(monoBin, "destroy", ".")
	cmd.Dir = envPath
	cmd.Run()

	db, err := OpenDB()
	if err == nil {
		db.DeleteEnvironment(envPath)
		db.Close()
	}

	home, _ := os.UserHomeDir()
	cacheDir := filepath.Join(home, ".mono", "cache_local")
	entries, _ := os.ReadDir(cacheDir)
	for _, entry := range entries {
		projectCacheDir := filepath.Join(cacheDir, entry.Name())
		info, err := os.Stat(projectCacheDir)
		if err == nil && info.IsDir() {
			expectedProjectID := ComputeProjectID(testDir)
			if entry.Name() == expectedProjectID {
				os.RemoveAll(projectCacheDir)
			}
		}
	}

	envName := filepath.Base(envPath)
	dataDir := filepath.Join(home, ".mono", "data", envName)
	os.RemoveAll(dataDir)

	sessionName := "mono-" + envName
	exec.Command("tmux", "kill-session", "-t", sessionName).Run()
}
