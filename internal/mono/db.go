package mono

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS environments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT UNIQUE NOT NULL,
    docker_project TEXT,
    root_path TEXT,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
`

const cacheEventsSchema = `
CREATE TABLE IF NOT EXISTS cache_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    event TEXT NOT NULL,
    project_id TEXT NOT NULL,
    artifact TEXT NOT NULL,
    cache_key TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cache_events_key ON cache_events(project_id, artifact, cache_key);
`

type DB struct {
	conn *sql.DB
	path string
}

func DBPath() (string, error) {
	var monoDir string

	if customHome := os.Getenv("MONO_HOME"); customHome != "" {
		monoDir = customHome
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		monoDir = filepath.Join(home, ".mono")
	}

	if err := os.MkdirAll(monoDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create mono directory: %w", err)
	}

	return filepath.Join(monoDir, "state.db"), nil
}

func OpenDB() (*DB, error) {
	dbPath, err := DBPath()
	if err != nil {
		return nil, err
	}

	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(0)

	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	db := &DB{conn: conn, path: dbPath}

	if err := db.Initialize(); err != nil {
		conn.Close()
		return nil, err
	}

	return db, nil
}

func (db *DB) Close() error {
	return db.conn.Close()
}

func (db *DB) Initialize() error {
	_, err := db.conn.Exec(schema)
	if err != nil {
		return fmt.Errorf("failed to create schema: %w", err)
	}

	db.conn.Exec(`ALTER TABLE environments ADD COLUMN root_path TEXT`)

	_, err = db.conn.Exec(cacheEventsSchema)
	if err != nil {
		return fmt.Errorf("failed to create cache_events schema: %w", err)
	}

	return nil
}

func (db *DB) RecordCacheEvent(event, projectID, artifact, cacheKey string) error {
	_, err := db.conn.Exec(
		`INSERT INTO cache_events (event, project_id, artifact, cache_key) VALUES (?, ?, ?, ?)`,
		event, projectID, artifact, cacheKey,
	)
	return err
}

type CacheEntry struct {
	ProjectID string
	Artifact  string
	CacheKey  string
	Hits      int
	Misses    int
	LastUsed  time.Time
}

func (db *DB) GetCacheStats() ([]CacheEntry, error) {
	rows, err := db.conn.Query(`
		SELECT
			project_id,
			artifact,
			cache_key,
			SUM(CASE WHEN event = 'hit' THEN 1 ELSE 0 END) as hits,
			SUM(CASE WHEN event = 'miss' THEN 1 ELSE 0 END) as misses,
			MAX(timestamp) as last_used
		FROM cache_events
		GROUP BY project_id, artifact, cache_key
		ORDER BY last_used DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []CacheEntry
	for rows.Next() {
		var e CacheEntry
		var lastUsedStr string
		if err := rows.Scan(&e.ProjectID, &e.Artifact, &e.CacheKey, &e.Hits, &e.Misses, &lastUsedStr); err != nil {
			return nil, err
		}
		lastUsed, err := time.Parse("2006-01-02 15:04:05", lastUsedStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse timestamp %q: %w", lastUsedStr, err)
		}
		e.LastUsed = lastUsed
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func (db *DB) DeleteCacheEvents(projectID, artifact, cacheKey string) error {
	_, err := db.conn.Exec(
		`DELETE FROM cache_events WHERE project_id = ? AND artifact = ? AND cache_key = ?`,
		projectID, artifact, cacheKey,
	)
	return err
}

func (db *DB) DeleteAllCacheEvents() error {
	_, err := db.conn.Exec(`DELETE FROM cache_events`)
	return err
}

func (db *DB) GetAllRootPaths() ([]string, error) {
	rows, err := db.conn.Query(`SELECT DISTINCT root_path FROM environments WHERE root_path IS NOT NULL AND root_path != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}
