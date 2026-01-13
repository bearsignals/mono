# mono recover

Restores environments after machine restart.

## Problem

Tmux sessions don't survive restarts. After reboot:
- SQLite state persists (knows what environments exist)
- Docker containers may or may not be running
- Tmux sessions are gone
- No way to restore them without full re-initialization

## Solution

```
mono recover <path>    Restore environment to running state
mono recover --all     Restore all registered environments
```

One command. Two modes.

---

## What "recover" means

Bring an environment back to the same state as after `mono init`, without:
- Re-running init/setup scripts
- Re-inserting into database
- Re-creating data directories

Just restore what's transient: Docker containers and tmux session.

---

## Command Details

### mono recover <path>

```
1. Look up environment by path → error if not registered
2. Derive names from path → project, workspace
3. Check tmux session exists
   - If yes → skip to step 5
4. Check Docker (if docker_project is set):
   a. If containers not running → start them (docker compose up -d)
   b. Get port mappings from running containers
5. Create tmux session with MONO_* env vars (including ports)
6. Done
```

Does NOT run any scripts (init, setup, run). User can `mono run` after.

### mono recover --all

```
1. Query all environments from database
2. For each environment, run recovery steps above
3. Report results (recovered, already running, failed)
```

---

## Environment Variable Reconstruction

The key challenge: reconstructing `MONO_*` variables for the tmux session.

| Variable | Source |
|----------|--------|
| `MONO_ENV_NAME` | Derived from path |
| `MONO_ENV_ID` | From database |
| `MONO_ENV_PATH` | The path argument |
| `MONO_ROOT_PATH` | From database (if set) |
| `MONO_DATA_DIR` | `~/.mono/data/<project>-<workspace>/` |
| `MONO_<SERVICE>_PORT` | Query running Docker containers |

Port reconstruction:
- If Docker containers are running → query actual mapped ports
- If Docker containers need starting → ports are deterministic from env_id

---

## Differences from init

| Step | init | recover |
|------|------|---------|
| Check path exists | Yes | Yes |
| Check already registered | Error if yes | Required |
| Insert into database | Yes | No (already exists) |
| Create data directory | Yes | No (already exists) |
| Run init script | Yes | No |
| Start Docker | Yes | Only if not running |
| Run setup script | Yes | No |
| Create tmux session | Yes | Only if not exists |
| Inject env vars | Yes | Yes |

---

## Edge Cases

| Scenario | Behavior |
|----------|----------|
| Path not in database | Error: "not registered, use mono init" |
| Tmux already running | No-op, print "already running" |
| Docker running, tmux dead | Start tmux only |
| Docker dead, tmux dead | Start Docker, then tmux |
| No docker_project (simple mode) | Start tmux only |
| Data dir missing | Warning, but continue |

---

## Implementation

### New files

None. Add to existing files.

### Changes to operations.go

```go
func Recover(path string, logger *FileLogger) error
func RecoverAll(logger *FileLogger) ([]RecoverResult, error)

type RecoverResult struct {
    Path    string
    Status  string  // "recovered", "already_running", "failed"
    Error   error
}
```

### Changes to cli/

Add `internal/cli/recover.go`:

```
mono recover <path>
mono recover --all
```

### Changes to docker.go

Add function to get ports from running containers:

```go
func GetRunningContainerPorts(projectName string) (map[string]int, error)
```

Uses `docker compose ps --format json` to query port mappings.

---

## CLI Output

### Single environment

```
$ mono recover /path/to/workspace
recovered frontend-feature-auth
```

Or if already running:

```
$ mono recover /path/to/workspace
frontend-feature-auth already running
```

### All environments

```
$ mono recover --all
recovered frontend-feature-auth
recovered backend-api
frontend-payments already running
```

---

## Error Messages

```
mono recover /unknown/path
error: environment not registered: /unknown/path
hint: use "mono init /unknown/path" to register

mono recover /path/to/workspace
error: failed to start docker containers: ...
```

---

## What recover does NOT do

- Run any scripts (init, setup, run, destroy)
- Modify database state
- Create data directories
- Handle partial Docker states (some containers up, some down)
- Migrate between mono versions

---

## Success Criteria

1. After machine restart, `mono recover --all` restores all environments
2. `mono run` works immediately after recover
3. No data loss or state corruption
4. Fast execution (seconds, not minutes)
