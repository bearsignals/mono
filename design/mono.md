# mono

A runtime backend for Conductor. Manages execution environments for workspaces.

## What mono does

- Starts Docker containers with isolated ports
- Creates tmux sessions for terminal access
- Manages data directories per environment
- Injects environment variables (MONO\_\*)
- Tracks state in SQLite

## What mono does NOT do

- Manage git worktrees (Conductor does this)
- Provide a web UI
- Orchestrate Claude instances
- Handle multi-project state

## Global CLI

mono is callable from anywhere. All commands take absolute paths as arguments - no dependency on current working directory.

```bash
# Works from any directory
mono init /path/to/workspace
mono run /path/to/workspace
mono list
```

## Logging

All operations are logged to `~/.mono/mono.log` with timestamps and environment context:

```
[14:32:05.123] [+0s] [auth-feature] mono init /Users/x/.conductor/workspaces/auth-feature
[14:32:05.456] [+333ms] [auth-feature] created data directory
[14:32:05.500] [+377ms] [auth-feature] running init script: npm install
[14:32:05.501] [+378ms] [auth-feature|out] npm install
[14:32:06.234] [+1.111s] [auth-feature|out] added 847 packages in 2.7s
[14:32:06.235] [+1.112s] [auth-feature] init script completed (exit 0)
[14:32:06.300] [+1.177s] [auth-feature] running: docker compose -p mono-auth-feature up -d
[14:32:06.512] [+1.389s] [auth-feature|out] Network mono-auth-feature Creating
[14:32:06.601] [+1.478s] [auth-feature|out] Network mono-auth-feature Created
[14:32:07.234] [+2.111s] [auth-feature|out] Container mono-auth-feature-db-1 Starting
[14:32:07.456] [+2.333s] [auth-feature|err] WARNING: image platform mismatch
[14:32:08.789] [+3.666s] [auth-feature|out] Container mono-auth-feature-db-1 Started
[14:32:08.790] [+3.667s] [auth-feature] docker compose completed (exit 0)
[14:32:09.012] [+3.889s] [auth-feature] created tmux session mono-auth-feature
```

**Streaming output with custom io.Writer:**

```go
type LogWriter struct {
    logger  *FileLogger
    envName string
    stream  string  // "out" or "err"
}

func (w *LogWriter) Write(p []byte) (n int, err error) {
    lines := strings.Split(string(p), "\n")
    for _, line := range lines {
        if line != "" {
            w.logger.Log("[%s|%s] %s", w.envName, w.stream, line)
        }
    }
    return len(p), nil
}
```

**Usage:**

```go
run.Command("docker", "compose", "up", "-d").
    Dir(workDir).
    Stdout(&LogWriter{logger: log, envName: envName, stream: "out"}).
    Stderr(&LogWriter{logger: log, envName: envName, stream: "err"}).
    Run()
```

**All command output is streamed and logged in real-time:**

- init/setup/destroy scripts
- docker compose up/down
- tmux commands

Useful for debugging - `tail -f ~/.mono/mono.log` shows live progress, grep by env name to filter.

---

## Commands

```
mono init <path>       Register environment, start containers, create tmux
mono destroy <path>    Stop containers, kill tmux, clean data
mono run <path>        Execute run script in tmux
mono list              List all environments
mono vars <path>       Print MONO_* environment variables
```

Five commands. No more.

---

## Data Model

### SQLite Schema

Location: `~/.mono/state.db`

```sql
CREATE TABLE environments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT UNIQUE NOT NULL,
    docker_project TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

One table. Ports are calculated deterministically from `env_id`, not stored.

### Directory Structure

```
~/.mono/
├── state.db                     # SQLite database
├── mono.log                     # Application log file
└── data/
    └── <env-name>/              # Per-environment data directory
```

---

## Configuration

### mono.yml

Located in workspace root. Optional.

```yaml
scripts:
  init: "npm install" # runs BEFORE docker starts
  setup: "npm run db:migrate" # runs AFTER docker is ready
  run: "npm run dev"
  destroy: "npm run cleanup"
```

Four scripts. No more.

### Environment Detection

**Docker mode** (workspace contains `docker-compose.yml`):

- Parse compose file
- Start containers with isolated ports, networks, volumes
- Generate `MONO_<SERVICE>_PORT` variables

**Simple mode** (no compose file):

- Skip all Docker management
- Still create tmux, data dir, env vars
- Users derive ports from `MONO_ENV_ID`:
  ```bash
  # mono.yml
  scripts:
    run: "npm run dev -- --port $((3000 + $MONO_ENV_ID))"
  ```

Both modes provide full isolation via:

- Unique `MONO_ENV_ID` for port derivation
- Unique `MONO_DATA_DIR` for file storage
- Unique tmux session

---

## Environment Variables

Injected into tmux session and available to scripts:

```
MONO_ENV_NAME      Environment name (derived from path)
MONO_ENV_ID        Unique integer ID (from SQLite)
MONO_ENV_PATH      Workspace path
MONO_DATA_DIR      Isolated data directory (~/.mono/data/<name>)
```

If Docker is used, per-service ports:

```
MONO_<SERVICE>_PORT    Host port mapped to service
```

Example: `MONO_WEB_PORT=19007`

---

## Port Allocation

Base port: `19000`

Formula: `base_port + (env_id * 100) + service_offset`

Example for env_id=3:

```
web (offset 0):  19300
api (offset 1):  19301
db (offset 2):   19302
```

Deterministic from `env_id` - no storage needed.

---

## Command Details

### mono init <path>

```
1. Check if path exists
2. Check if already registered → error if yes
3. Insert into environments table → get env_id
4. Create data directory: ~/.mono/data/<name>
5. If mono.yml has init script, run it
6. If docker-compose.yml exists (Docker mode):
   a. Parse compose file
   b. Allocate ports for each service
   c. Generate docker-compose.mono.yml with port overrides
   d. Run: docker compose -p mono-<name> up -d
7. If mono.yml has setup script, run it
8. Create tmux session: mono-<name>
9. Export MONO_* variables to tmux
```

**Simple mode**: Steps 6a-6d are skipped. No `MONO_<SERVICE>_PORT` vars, but `MONO_ENV_ID` is available for manual port derivation.

### mono destroy <path>

```
1. Look up environment by path → error if not found
2. If mono.yml has destroy script, run it (may need docker for db dumps, etc.)
3. Kill tmux session: mono-<name> (stops processes that depend on docker)
4. If Docker was used (docker_project is set):
   a. Run: docker compose -p mono-<name> down -v
5. Remove data directory: ~/.mono/data/<name>
6. Delete from environments table
```

**Simple mode**: Step 4 is skipped.

### mono run <path>

```
1. Look up environment by path → error if not found
2. Read mono.yml for run script → error if no script
3. Send command to tmux: tmux send-keys -t mono-<name> "<script>" Enter
```

### mono list

```
1. Query all environments
2. For each, check:
   - tmux session exists?
   - Docker containers running?
3. Print table:
   NAME          PATH                                    STATUS
   auth-feature  ~/.conductor/workspaces/auth-feature    running
   payments      ~/.conductor/workspaces/payments        stopped
```

### mono vars <path>

```
1. Look up environment by path
2. If docker_project is set, calculate service ports from env_id
3. Print:
   export MONO_ENV_NAME="auth-feature"
   export MONO_ENV_ID="3"
   export MONO_ENV_PATH="/Users/x/.conductor/workspaces/auth-feature"
   export MONO_DATA_DIR="/Users/x/.mono/data/auth-feature"
   export MONO_WEB_PORT="19300"  # only in Docker mode
```

---

## Conductor Integration

### conductor.json

```json
{
  "scripts": {
    "setup": "mono init \"$CONDUCTOR_ROOT_PATH\"",
    "run": "mono run \"$CONDUCTOR_ROOT_PATH\"",
    "archive": "mono destroy \"$CONDUCTOR_ROOT_PATH\""
  }
}
```

### Flow

```
Conductor creates workspace (worktree)
         ↓
Conductor setup script: mono init /path/to/workspace
         ↓
         ├── 1. Register in SQLite, create data dir
         ├── 2. Run mono.yml init script (npm install, etc.)
         ├── 3. Start docker containers with isolated ports/volumes/networks
         ├── 4. Run mono.yml setup script (db migrations, etc.)
         └── 5. Create tmux session with MONO_* vars
         ↓
Claude Code runs in workspace with full isolation
         ↓
Conductor run button: mono run /path/to/workspace
         ↓
mono: sends run script to tmux session
         ↓
User archives workspace
         ↓
Conductor archive script: mono destroy /path/to/workspace
         ↓
         ├── 1. Run mono.yml destroy script (may need docker)
         ├── 2. Kill tmux session (stops processes using docker)
         ├── 3. Stop docker containers, remove volumes
         └── 4. Remove data dir, delete from SQLite
         ↓
Conductor deletes worktree
```

---

## Project Structure

```
mono/
├── cmd/mono/main.go
├── internal/
│   ├── cli/
│   │   ├── root.go
│   │   ├── init.go
│   │   ├── destroy.go
│   │   ├── run.go
│   │   ├── list.go
│   │   └── vars.go
│   └── mono/
│       ├── db.go           # database connection, schema
│       ├── environment.go  # environment CRUD queries
│       ├── docker.go       # compose parsing, container lifecycle
│       ├── tmux.go         # session management
│       ├── ports.go        # port allocation
│       ├── config.go       # mono.yml parsing
│       ├── env.go          # MONO_* variable generation
│       ├── logger.go       # FileLogger + LogWriter
│       ├── cmd.go          # command execution wrapper
│       └── operations.go   # Init(), Destroy(), Run()
├── go.mod
└── go.sum
```

Two packages: `cli` for commands, `mono` for everything else.

---

## Dependencies

```
github.com/spf13/cobra           CLI framework
modernc.org/sqlite               SQLite (CGO-free)
github.com/compose-spec/compose-go/v2   Docker Compose parsing
gopkg.in/yaml.v3                 YAML parsing
```

---

## Scope Boundaries

### Will NOT add

- Web UI
- WebSocket/real-time features
- Git operations
- Project-level abstractions
- Additional script hooks beyond init/run/destroy
- Custom tmux window management
- Shared services between environments

### May add later (only if needed)

- `mono attach <path>` - attach to tmux session
- `mono logs <path>` - view container logs
- `mono exec <path> <cmd>` - run command in container

---

## Implementation Order

1. SQLite state management (state/)
2. Port allocation (ports/)
3. Docker lifecycle (docker/)
4. Tmux session management (tmux/)
5. Environment variables (env/)
6. Config parsing (config/)
7. Operations layer (operations/)
8. CLI commands (cli/)

---

## Error Handling

All errors are returned, never swallowed.

```go
func Init(path string) error {
    if _, err := os.Stat(path); err != nil {
        return fmt.Errorf("path does not exist: %s", path)
    }

    exists, err := state.EnvironmentExists(path)
    if err != nil {
        return fmt.Errorf("failed to check environment: %w", err)
    }
    if exists {
        return fmt.Errorf("environment already exists: %s", path)
    }

    // ...
}
```

---

## Success Criteria

mono is complete when:

1. `mono init` creates a fully isolated environment
2. `mono run` executes the dev server
3. `mono destroy` cleanly tears everything down
4. Multiple environments can run simultaneously with no port conflicts
5. State persists across mono restarts
6. Conductor integration works via scripts

---

## Code Reuse from Piko

### Source Location

Piko codebase: `/Users/gwuah/Desktop/piko`

### Mapping Table

| mono file             | piko source                     | reuse         | modifications                      |
| --------------------- | ------------------------------- | ------------- | ---------------------------------- |
| `mono/cmd.go`         | `internal/run/cmd.go`           | Copy directly | None                               |
| `mono/ports.go`       | `internal/ports/allocator.go`   | Copy directly | Update base port constant          |
| `mono/db.go`          | `internal/state/db.go`          | Adapt         | New schema, ~/.mono/state.db       |
| `mono/environment.go` | `internal/state/environment.go` | Adapt         | Remove Project concept             |
| `mono/docker.go`      | `internal/docker/*.go`          | Adapt         | Keep override logic, rename prefix |
| `mono/tmux.go`        | `internal/tmux/session.go`      | Adapt         | Remove piko naming, simplify       |
| `mono/env.go`         | `internal/env/vars.go`          | Adapt         | PIKO* → MONO* prefix               |
| `mono/config.go`      | `internal/config/piko.go`       | Adapt         | .piko.yml → mono.yml               |
| `mono/logger.go`      | `internal/logger/logger.go`     | Adapt         | Add LogWriter, ~/.mono/mono.log    |
| `mono/operations.go`  | `internal/operations/*.go`      | Adapt         | Init(), Destroy(), Run()           |

---

### Package Details

All files below are in `internal/mono/` package.

#### cmd.go (Copy as-is)

**Source:** `piko/internal/run/cmd.go`

```go
Command(name string, args ...string) *Cmd
Cmd.Dir(dir string) *Cmd
Cmd.Timeout(d time.Duration) *Cmd
Cmd.Stdout(w io.Writer) *Cmd
Cmd.Stderr(w io.Writer) *Cmd
Cmd.Run() error
```

No changes needed. Use `Stdout()` / `Stderr()` with `LogWriter` for streaming.

---

#### ports.go (Copy, minor edits)

**Source:** `piko/internal/ports/allocator.go`

```go
Allocation struct { Service, ContainerPort, HostPort }
Allocate(envID int64, servicePorts map[string][]uint32) []Allocation
```

**Changes:** Update `BasePort` from 10000 → 19000

---

#### db.go + environment.go (Adapt)

**Source:** `piko/internal/state/db.go`, `environment.go`

**db.go:**

```go
Open() (*DB, error)  // opens ~/.mono/state.db
Initialize() error   // creates schema
```

**environment.go:**

```go
InsertEnvironment(path, dockerProject string) (int64, error)
GetEnvironmentByPath(path string) (*Environment, error)
ListEnvironments() ([]*Environment, error)
DeleteEnvironment(path string) error
```

**Schema:**

```sql
CREATE TABLE environments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT UNIQUE NOT NULL,
    docker_project TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

---

#### docker.go (Adapt)

**Source:** `piko/internal/docker/compose.go`, `override.go`

```go
CheckDockerAvailable() error
DetectComposeFile(dir string) (string, error)
ParseComposeConfig(workDir string) (*ComposeConfig, error)
GetServicePorts() map[string][]uint32
ApplyOverrides(project, envName string, allocations []Allocation)
StartContainers(projectName, workDir string) error
StopContainers(projectName string) error
```

**Keep ApplyOverrides()** - essential for isolation (ports, networks, volumes).

**Changes:** Rename prefix `piko-<project>-<env>` → `mono-<env>`

---

#### tmux.go (Adapt)

**Source:** `piko/internal/tmux/session.go`

```go
SessionExists(name string) (bool, error)
CreateSession(name, workDir string) error
SendKeys(session, keys string) error
KillSession(name string) error
```

**Remove:** `ListPikoSessions()`, `CreateFullSession()`, window management

**Naming:** Sessions are `mono-<env-name>`

---

#### env.go (Adapt)

**Source:** `piko/internal/env/vars.go`

```go
type MonoEnv struct {
    EnvName  string
    EnvID    int64
    EnvPath  string
    DataDir  string
    Ports    map[string]int
}

func (e *MonoEnv) ToEnvSlice() []string
func (e *MonoEnv) ToShellExport() string
```

**Changes:** `PIKO_*` → `MONO_*`, remove project fields

---

#### config.go (Adapt)

**Source:** `piko/internal/config/piko.go`

```go
type Config struct {
    Scripts Scripts `yaml:"scripts"`
}

type Scripts struct {
    Init    string `yaml:"init"`
    Setup   string `yaml:"setup"`
    Run     string `yaml:"run"`
    Destroy string `yaml:"destroy"`
}

func Load(dir string) (*Config, error)
```

**Remove:** `Shells`, `Shared`, `Ignore`

---

#### logger.go (Adapt)

**Source:** `piko/internal/logger/logger.go`

```go
type FileLogger struct { ... }
func NewFileLogger(path string) (*FileLogger, error)
func (l *FileLogger) Log(format string, args ...any)
func (l *FileLogger) Close()

type LogWriter struct {
    logger  *FileLogger
    envName string
    stream  string  // "out" or "err"
}
func (w *LogWriter) Write(p []byte) (n int, err error)
```

**Changes:** Log to `~/.mono/mono.log`, add `LogWriter`

---

#### operations.go (New)

**Source:** `piko/internal/operations/*.go`

```go
func Init(path string, logger *FileLogger) error
func Destroy(path string, logger *FileLogger) error
func Run(path string, logger *FileLogger) error
```

Orchestrates all other components.

---

### Dependency Versions (from piko go.mod)

```go
require (
    github.com/spf13/cobra v1.9.1
    github.com/compose-spec/compose-go/v2 v2.4.7
    gopkg.in/yaml.v3 v3.0.1
    modernc.org/sqlite v1.42.2
)
```

---

### Implementation Checklist

```
[ ] 1. Create mono/ directory structure
[ ] 2. Initialize go.mod with dependencies
[ ] 3. Create internal/mono/cmd.go (copy from piko)
[ ] 4. Create internal/mono/ports.go (update BasePort)
[ ] 5. Create internal/mono/logger.go (add LogWriter)
[ ] 6. Create internal/mono/db.go (new schema)
[ ] 7. Create internal/mono/environment.go
[ ] 8. Create internal/mono/docker.go (rename prefix)
[ ] 9. Create internal/mono/tmux.go (simplify)
[ ] 10. Create internal/mono/env.go (rename prefix)
[ ] 11. Create internal/mono/config.go (simplify)
[ ] 12. Create internal/mono/operations.go
[ ] 13. Create internal/cli/ (5 commands)
[ ] 14. Create cmd/mono/main.go
[ ] 15. Test with Conductor
```
