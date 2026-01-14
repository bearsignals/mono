package mono

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func Init(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("path does not exist: %s", path)
	}

	project, workspace := DeriveNames(path)
	envName := fmt.Sprintf("%s-%s", project, workspace)
	if project == "" || workspace == "" {
		envName = filepath.Base(path)
	}

	logger, err := NewFileLogger(envName)
	if err != nil {
		return fmt.Errorf("failed to create logger: %w", err)
	}
	defer logger.Close()

	logger.Log("mono init %s", path)

	db, err := OpenDB()
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	exists, err := db.EnvironmentExists(path)
	if err != nil {
		return fmt.Errorf("failed to check environment: %w", err)
	}
	if exists {
		return fmt.Errorf("environment already exists: %s", path)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}
	dataDir := filepath.Join(home, ".mono", "data", envName)
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}
	logger.Log("created data directory")

	cleanup := func() {
		os.RemoveAll(dataDir)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		cleanup()
		return fmt.Errorf("failed to load config: %w", err)
	}
	cfg.ApplyDefaults(path)

	cm, err := NewCacheManager()
	if err != nil {
		cleanup()
		return fmt.Errorf("failed to initialize cache: %w", err)
	}

	if err := cm.EnsureDirectories(); err != nil {
		cleanup()
		return fmt.Errorf("failed to create cache directories: %w", err)
	}

	if cm.SccacheAvailable {
		logger.Log("sccache detected, compilation caching enabled")
	} else {
		logger.Log("sccache not found, compilation caching disabled")
		logger.Log("hint: install sccache for faster builds: cargo install sccache")
	}

	rootPath := os.Getenv("CONDUCTOR_ROOT_PATH")

	var cacheEntries []ArtifactCacheEntry
	if len(cfg.Build.Artifacts) > 0 && rootPath != "" {
		entries, err := cm.PrepareArtifactCache(cfg.Build.Artifacts, rootPath, path)
		if err != nil {
			logger.Log("warning: failed to prepare artifact cache: %v", err)
		} else {
			cacheEntries = entries
		}

		hasMiss := false
		for _, entry := range cacheEntries {
			if !entry.Hit {
				hasMiss = true
				break
			}
		}

		if hasMiss {
			if err := cm.SeedFromRoot(cfg.Build.Artifacts, rootPath, path); err != nil {
				logger.Log("warning: failed to seed cache from root: %v", err)
			} else {
				logger.Log("attempted to seed cache from project root")
			}

			entries, err := cm.PrepareArtifactCache(cfg.Build.Artifacts, rootPath, path)
			if err != nil {
				logger.Log("warning: failed to re-prepare artifact cache: %v", err)
			} else {
				cacheEntries = entries
			}
		}

		projectID := ComputeProjectID(rootPath)
		for i := range cacheEntries {
			entry := &cacheEntries[i]
			if entry.Hit {
				logger.Log("cache hit for %s (key: %s)", entry.Name, entry.Key)
				if err := cm.RestoreFromCache(*entry); err != nil {
					logger.Log("warning: failed to restore cache: %v", err)
					entry.Hit = false
				} else {
					if err := db.RecordCacheEvent("hit", projectID, entry.Name, entry.Key); err != nil {
						logger.Log("warning: failed to record cache hit: %v", err)
					}
				}
			} else {
				logger.Log("cache miss for %s (key: %s)", entry.Name, entry.Key)
				if err := db.RecordCacheEvent("miss", projectID, entry.Name, entry.Key); err != nil {
					logger.Log("warning: failed to record cache miss: %v", err)
				}
			}
		}
	}

	allHit := true
	for _, entry := range cacheEntries {
		if !entry.Hit {
			allHit = false
			break
		}
	}

	cacheEnvVars := cm.EnvVars(cfg.Build)
	cacheEnvVars = append(cacheEnvVars, fmt.Sprintf("MONO_CACHE_HIT=%t", allHit))
	cacheEnvVars = append(cacheEnvVars, "MONO_CACHE_DIR="+cm.LocalCacheDir)

	_, composeErr := DetectComposeFile(path)
	isSimpleMode := composeErr != nil

	dockerProject := ""
	if !isSimpleMode {
		dockerProject = fmt.Sprintf("mono-%s", envName)
	}

	envID, err := db.InsertEnvironment(path, dockerProject, rootPath)
	if err != nil {
		cleanup()
		return fmt.Errorf("failed to save environment: %w", err)
	}
	logger.Log("registered environment (id=%d)", envID)

	cleanupWithDB := func() {
		db.DeleteEnvironment(path)
		cleanup()
	}

	var allocations []Allocation

	if cfg.Scripts.Init != "" {
		monoEnv := BuildEnv(envName, envID, path, rootPath, allocations)
		logger.Log("running init script: %s", cfg.Scripts.Init)
		if err := runScript(path, cfg.Scripts.Init, monoEnv.ToEnvSlice(), cacheEnvVars, logger); err != nil {
			cleanupWithDB()
			return fmt.Errorf("init script failed: %w", err)
		}
		logger.Log("init script completed")
	}

	for i := range cacheEntries {
		entry := &cacheEntries[i]
		if !entry.Hit {
			if err := cm.StoreToCache(*entry); err != nil {
				logger.Log("warning: failed to store %s to cache: %v", entry.Name, err)
			} else {
				logger.Log("stored %s to cache (key: %s)", entry.Name, entry.Key)
				entry.Hit = true
			}
		}
	}

	if !isSimpleMode {
		if err := CheckDockerAvailable(); err != nil {
			cleanupWithDB()
			return err
		}

		composeConfig, err := ParseComposeConfig(path)
		if err != nil {
			cleanupWithDB()
			return fmt.Errorf("failed to parse compose config: %w", err)
		}

		servicePorts := composeConfig.GetServicePorts()
		allocations = Allocate(envID, servicePorts)

		composeProject := composeConfig.Project()
		ApplyOverrides(composeProject, envName, allocations)

		monoComposePath := filepath.Join(path, "docker-compose.mono.yml")
		if err := WriteComposeOverride(monoComposePath, composeProject); err != nil {
			cleanupWithDB()
			return fmt.Errorf("failed to write compose override: %w", err)
		}
		logger.Log("generated docker-compose.mono.yml")

		logger.Log("running: docker compose -p %s up -d", dockerProject)
		stdout := NewLogWriter(logger, "out")
		stderr := NewLogWriter(logger, "err")
		if err := StartContainers(dockerProject, path, stdout, stderr); err != nil {
			cleanupWithDB()
			return fmt.Errorf("failed to start containers: %w", err)
		}
		logger.Log("docker compose completed")
	}

	if cfg.Scripts.Setup != "" {
		monoEnv := BuildEnv(envName, envID, path, rootPath, allocations)
		logger.Log("running setup script: %s", cfg.Scripts.Setup)
		if err := runScript(path, cfg.Scripts.Setup, monoEnv.ToEnvSlice(), cacheEnvVars, logger); err != nil {
			if !isSimpleMode {
				StopContainers(dockerProject, path, true, nil, nil)
			}
			cleanupWithDB()
			return fmt.Errorf("setup script failed: %w", err)
		}
		logger.Log("setup script completed")
	}

	monoEnv := BuildEnv(envName, envID, path, rootPath, allocations)
	sessionName := SessionName(envName)
	if err := CreateSession(sessionName, path, monoEnv.ToEnvSlice()); err != nil {
		logger.Log("warning: failed to create tmux session: %v", err)
	} else {
		logger.Log("created tmux session %s", sessionName)
	}

	fmt.Printf("Environment initialized: %s\n", envName)
	fmt.Printf("  Path: %s\n", path)
	fmt.Printf("  Data: %s\n", dataDir)
	if !isSimpleMode {
		fmt.Printf("  Docker: %s\n", dockerProject)
		for _, alloc := range allocations {
			fmt.Printf("  %s: %d -> %d\n", alloc.Service, alloc.ContainerPort, alloc.HostPort)
		}
	}
	fmt.Printf("  Tmux: %s\n", sessionName)

	return nil
}

func Destroy(path string) error {
	project, workspace := DeriveNames(path)
	envName := fmt.Sprintf("%s-%s", project, workspace)
	if project == "" || workspace == "" {
		envName = filepath.Base(path)
	}

	logger, err := NewFileLogger(envName)
	if err != nil {
		return fmt.Errorf("failed to create logger: %w", err)
	}
	defer logger.Close()

	logger.Log("mono destroy %s", path)

	db, err := OpenDB()
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	env, err := db.GetEnvironmentByPath(path)
	if err != nil {
		return fmt.Errorf("environment not found: %s", path)
	}

	cfg, _ := LoadConfig(path)

	rootPath := ""
	if env.RootPath.Valid {
		rootPath = env.RootPath.String
	}

	if cfg != nil && rootPath != "" {
		cfg.ApplyDefaults(path)
		cm, err := NewCacheManager()
		if err == nil {
			if err := cm.Sync(cfg.Build.Artifacts, rootPath, path, SyncOptions{HardlinkBack: false}); err != nil {
				logger.Log("warning: failed to sync before destroy: %v", err)
			} else {
				logger.Log("synced artifacts to cache before destroy")
			}
		}
	}

	if cfg != nil && cfg.Scripts.Destroy != "" {
		monoEnv := BuildEnv(envName, env.ID, path, rootPath, nil)
		logger.Log("running destroy script: %s", cfg.Scripts.Destroy)
		if err := runScript(path, cfg.Scripts.Destroy, monoEnv.ToEnvSlice(), nil, logger); err != nil {
			logger.Log("warning: destroy script failed: %v", err)
		} else {
			logger.Log("destroy script completed")
		}
	}

	sessionName := SessionName(envName)
	if SessionExists(sessionName) {
		if err := KillSession(sessionName); err != nil {
			logger.Log("warning: failed to kill tmux session: %v", err)
		} else {
			logger.Log("killed tmux session %s", sessionName)
		}
	}

	if env.DockerProject.Valid && env.DockerProject.String != "" {
		logger.Log("stopping containers: %s", env.DockerProject.String)
		stdout := NewLogWriter(logger, "out")
		stderr := NewLogWriter(logger, "err")
		if err := StopContainers(env.DockerProject.String, path, true, stdout, stderr); err != nil {
			logger.Log("warning: failed to stop containers: %v", err)
		} else {
			logger.Log("stopped containers")
		}
	}

	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, ".mono", "data", envName)
	if err := os.RemoveAll(dataDir); err != nil {
		logger.Log("warning: failed to remove data directory: %v", err)
	} else {
		logger.Log("removed data directory")
	}

	if err := db.DeleteEnvironment(path); err != nil {
		return fmt.Errorf("failed to delete environment: %w", err)
	}
	logger.Log("removed from database")

	fmt.Printf("Environment destroyed: %s\n", envName)
	return nil
}

func Run(path string) error {
	project, workspace := DeriveNames(path)
	envName := fmt.Sprintf("%s-%s", project, workspace)
	if project == "" || workspace == "" {
		envName = filepath.Base(path)
	}

	logger, err := NewFileLogger(envName)
	if err != nil {
		return fmt.Errorf("failed to create logger: %w", err)
	}
	defer logger.Close()

	logger.Log("mono run %s", path)

	db, err := OpenDB()
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	_, err = db.GetEnvironmentByPath(path)
	if err != nil {
		return fmt.Errorf("environment not found: %s", path)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if cfg.Scripts.Run == "" {
		return fmt.Errorf("no run script defined in mono.yml")
	}

	sessionName := SessionName(envName)
	if !SessionExists(sessionName) {
		return fmt.Errorf("tmux session does not exist: %s", sessionName)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}
	dataDir := filepath.Join(home, ".mono", "data", envName)
	scriptPath := filepath.Join(dataDir, "run.sh")

	if err := os.WriteFile(scriptPath, []byte(cfg.Scripts.Run), 0755); err != nil {
		return fmt.Errorf("failed to write run script: %w", err)
	}

	logger.Log("sending to tmux: source %s", scriptPath)
	if err := SendKeys(sessionName, "source "+scriptPath); err != nil {
		return fmt.Errorf("failed to send keys to tmux: %w", err)
	}

	fmt.Printf("Session: %s\n", sessionName)
	return nil
}

type EnvironmentStatus struct {
	Name          string
	Path          string
	TmuxRunning   bool
	DockerRunning bool
}

func List() ([]EnvironmentStatus, error) {
	db, err := OpenDB()
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	environments, err := db.ListEnvironments()
	if err != nil {
		return nil, fmt.Errorf("failed to list environments: %w", err)
	}

	var statuses []EnvironmentStatus
	for _, env := range environments {
		project, workspace := DeriveNames(env.Path)
		envName := fmt.Sprintf("%s-%s", project, workspace)
		if project == "" || workspace == "" {
			envName = filepath.Base(env.Path)
		}

		sessionName := SessionName(envName)
		tmuxRunning := SessionExists(sessionName)

		dockerRunning := false
		if env.DockerProject.Valid && env.DockerProject.String != "" {
			dockerRunning = ContainersRunning(env.DockerProject.String)
		}

		statuses = append(statuses, EnvironmentStatus{
			Name:          envName,
			Path:          env.Path,
			TmuxRunning:   tmuxRunning,
			DockerRunning: dockerRunning,
		})
	}

	return statuses, nil
}

func runScript(workDir, script string, envVars []string, extraEnvVars []string, logger *FileLogger) error {
	stdout := NewLogWriter(logger, "out")
	stderr := NewLogWriter(logger, "err")

	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = workDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), envVars...)
	cmd.Env = append(cmd.Env, extraEnvVars...)

	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Minute):
		cmd.Process.Kill()
		return fmt.Errorf("script timed out after 10 minutes")
	}
}
