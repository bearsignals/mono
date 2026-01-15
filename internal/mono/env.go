package mono

import (
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

