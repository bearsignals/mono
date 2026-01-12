package mono

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func DeriveNames(path string) (project, workspace string) {
	parts := strings.Split(path, string(filepath.Separator))
	for i, part := range parts {
		if part == "workspaces" && i < len(parts)-2 {
			project = parts[i+1]
			workspace = parts[i+2]
			return project, workspace
		}
	}
	return project, workspace
}

type MonoEnv struct {
	EnvName string
	EnvID   int64
	EnvPath string
	DataDir string
	Ports   map[string]int
}

func BuildEnv(envName string, envID int64, envPath string, allocations []Allocation) *MonoEnv {
	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, ".mono", "data", envName)

	ports := make(map[string]int)
	for _, alloc := range allocations {
		varName := serviceToVarName(alloc.Service)
		ports[varName] = alloc.HostPort
	}

	return &MonoEnv{
		EnvName: envName,
		EnvID:   envID,
		EnvPath: envPath,
		DataDir: dataDir,
		Ports:   ports,
	}
}

func serviceToVarName(service string) string {
	upper := strings.ToUpper(service)
	normalized := strings.ReplaceAll(upper, "-", "_")
	return "MONO_" + normalized + "_PORT"
}

func (e *MonoEnv) FullName() string {
	return e.EnvName
}

func (e *MonoEnv) ToEnvSlice() []string {
	vars := []string{
		fmt.Sprintf("MONO_ENV_NAME=%s", e.EnvName),
		fmt.Sprintf("MONO_ENV_ID=%d", e.EnvID),
		fmt.Sprintf("MONO_ENV_PATH=%s", e.EnvPath),
		fmt.Sprintf("MONO_DATA_DIR=%s", e.DataDir),
	}

	for name, port := range e.Ports {
		vars = append(vars, fmt.Sprintf("%s=%d", name, port))
	}

	return vars
}
