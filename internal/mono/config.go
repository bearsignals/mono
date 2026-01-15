package mono

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

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

type Config struct {
	Scripts Scripts           `yaml:"scripts"`
	Build   BuildConfig       `yaml:"build"`
	Env     map[string]string `yaml:"env"`
}

type Scripts struct {
	Init    string `yaml:"init"`
	Setup   string `yaml:"setup"`
	Run     string `yaml:"run"`
	Destroy string `yaml:"destroy"`
}

func LoadConfig(dir string) (*Config, error) {
	path := filepath.Join(dir, "mono.yml")

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read mono.yml: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid mono.yml: %w", err)
	}

	return &cfg, nil
}

func (c *Config) ApplyDefaults(envPath string) {
	if len(c.Build.Artifacts) == 0 {
		c.Build.Artifacts = detectArtifacts(envPath)
	}
}

type lockFileSpec struct {
	filename    string
	artifactDir string
	keyCommand  string
	baseType    string
}

var lockFileSpecs = []lockFileSpec{
	{"Cargo.lock", "target", "rustc --version", "cargo"},
	{"package-lock.json", "node_modules", "node --version", "npm"},
	{"yarn.lock", "node_modules", "node --version", "yarn"},
	{"pnpm-lock.yaml", "node_modules", "node --version", "pnpm"},
	{"bun.lock", "node_modules", "bun --version", "bun"},
	{"bun.lockb", "node_modules", "bun --version", "bun"},
}

var skipDirs = map[string]bool{
	"node_modules": true,
	"target":       true,
	".git":         true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".next":        true,
	".nuxt":        true,
}

func detectArtifacts(envPath string) []ArtifactConfig {
	var artifacts []ArtifactConfig
	lockFiles := findLockFiles(envPath)

	seen := make(map[string]bool)
	for _, lf := range lockFiles {
		cfg := lf.toArtifactConfig()
		if seen[cfg.Name] {
			continue
		}
		seen[cfg.Name] = true
		artifacts = append(artifacts, cfg)
	}

	return artifacts
}

type foundLockFile struct {
	relPath  string
	spec     lockFileSpec
}

func (f foundLockFile) toArtifactConfig() ArtifactConfig {
	dir := filepath.Dir(f.relPath)
	name := f.spec.baseType
	artifactPath := f.spec.artifactDir

	if dir != "." {
		name = f.spec.baseType + "-" + sanitizeName(dir)
		artifactPath = filepath.Join(dir, f.spec.artifactDir)
	}

	return ArtifactConfig{
		Name:        name,
		KeyFiles:    []string{f.relPath},
		KeyCommands: []string{f.spec.keyCommand},
		Paths:       []string{artifactPath},
	}
}

func sanitizeName(dir string) string {
	name := strings.ReplaceAll(dir, string(filepath.Separator), "-")
	name = strings.ReplaceAll(name, ".", "-")
	return strings.ToLower(name)
}

func findLockFiles(envPath string) []foundLockFile {
	var found []foundLockFile
	specMap := make(map[string]lockFileSpec)
	for _, spec := range lockFileSpecs {
		specMap[spec.filename] = spec
	}

	filepath.WalkDir(envPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		spec, ok := specMap[d.Name()]
		if !ok {
			return nil
		}

		relPath, err := filepath.Rel(envPath, path)
		if err != nil {
			return nil
		}

		found = append(found, foundLockFile{
			relPath: relPath,
			spec:    spec,
		})

		return nil
	})

	return found
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
