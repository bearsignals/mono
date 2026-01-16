# mono

mono is a backend for conductor.build, that allows developers to create parallel and isolated development environments for each conductor workspace.

## What mono does

- Starts Docker containers with isolated ports
- Creates tmux sessions for terminal access
- Manages data directories per environment
- Injects environment variables (MONO\_\*)
- Tracks state in SQLite


## Usage


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

Useful for debugging - `tail -f ~/.mono/mono.log` shows live progress, grep by env name to filter.

---

## Commands

```
mono init <path>       Register environment, start containers, create tmux
mono destroy <path>    Stop containers, kill tmux, clean data
mono run <path>        Execute run script in tmux
mono list              List all environments
mono cache stats       Shows statistics for internal cache maintained by mono
mono cache clean -all  Cleans up the entire cache, too free up space.
```

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
    └── <project>-<workspace>/   # Per-environment data directory
```

---

## Configuration

### mono.yml

Located in workspace root. Optional.

```yaml
env:
  MONO_HOME: "${MONO_DATA_DIR}"

scripts:
  init: "npm install" # runs BEFORE docker starts
  setup: "npm run db:migrate" # runs AFTER docker is ready
  run: "go run ./cmd/mono list"
  destroy: "npm run cleanup"

```

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
3. Derive names from path → project, workspace (e.g., "frontend", "feature-auth")
4. Insert into environments table → get env_id (for port allocation)
5. Create data directory: ~/.mono/data/<project>-<workspace>/
6. If mono.yml has init script, run it
7. If docker-compose.yml exists (Docker mode):
   a. Parse compose file
   b. Allocate ports for each service (using env_id)
   c. Generate docker-compose.mono.yml with port overrides
   d. Run: docker compose -p mono-<project>-<workspace> up -d
8. If mono.yml has setup script, run it
9. Create tmux session: mono-<project>-<workspace>
10. Export MONO_* variables to tmux
```

**Simple mode**: Steps 7a-7d are skipped. No `MONO_<SERVICE>_PORT` vars, but `MONO_ENV_ID` is available for manual port derivation.

### mono destroy <path>

```
1. Look up environment by path → error if not found
2. Derive names from path → project, workspace
3. If mono.yml has destroy script, run it (may need docker for db dumps, etc.)
4. Kill tmux session: mono-<project>-<workspace> (stops processes that depend on docker)
5. If Docker was used (docker_project is set):
   a. Run: docker compose -p mono-<project>-<workspace> down -v
6. Remove data directory: ~/.mono/data/<project>-<workspace>/
7. Delete from environments table
```

**Simple mode**: Step 5 is skipped.

### mono run <path>

```
1. Look up environment by path → error if not found
2. Derive names from path → project, workspace
3. Read mono.yml for run script → error if no script
4. Send command to tmux: tmux send-keys -t mono-<project>-<workspace> "<script>" Enter
```

### mono list

```
1. Query all environments
2. For each, derive names and check:
   - tmux session exists?
   - Docker containers running?
3. Print table:
   NAME                      PATH                                         STATUS
   frontend-feature-auth     ~/frontend/.conductor/workspaces/feature-auth    running
   backend-feature-auth      ~/backend/.conductor/workspaces/feature-auth     running
   frontend-payments         ~/frontend/.conductor/workspaces/payments        stopped
```

---

## Conductor Integration

### conductor.json

```json
{
  "scripts": {
    "setup": "mono init",
    "run": "mono run",
    "archive": "mono destroy"
  }
}
```

### Flow

```
Conductor creates workspace (worktree)
         ↓
Conductor setup script: mono init /path/to/frontend/.conductor/workspaces/feature-auth
         ↓
         ├── 1. Derive names: project="frontend", workspace="feature-auth"
         ├── 2. Register in SQLite (get env_id for ports), create data dir
         ├── 3. Run mono.yml init script (npm install, etc.)
         ├── 4. Start docker containers (mono-frontend-feature-auth) with isolated ports/volumes/networks
         ├── 5. Run mono.yml setup script (db migrations, etc.)
         └── 6. Create tmux session (mono-frontend-feature-auth) with MONO_* vars
         ↓
Claude Code runs in workspace with full isolation
         ↓
Conductor run button: mono run /path/to/workspace
         ↓
mono: sends run script to tmux session mono-frontend-feature-auth
         ↓
User archives workspace
         ↓
Conductor archive script: mono destroy /path/to/workspace
         ↓
         ├── 1. Run mono.yml destroy script (may need docker)
         ├── 2. Kill tmux session mono-frontend-feature-auth
         ├── 3. Stop docker containers mono-frontend-feature-auth, remove volumes
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
│   │   └── list.go
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

Two packages: `cli` (5 files), `mono` (10 files).

---

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