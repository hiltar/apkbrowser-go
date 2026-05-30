package database

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
        "strings"
	"sync"

	_ "modernc.org/sqlite" // Pure Go SQLite driver
)

type Manager struct {
	connections map[string]*sql.DB
	mu          sync.RWMutex
}

func Open(dbDir string, branches []string) (*Manager, error) {
	// Ensure database directory exists
	absDir, _ := filepath.Abs(dbDir)
	log.Printf("Database directory resolved to: %s", absDir)
	if err := os.MkdirAll(absDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory %s: %w", absDir, err)
	}

	m := &Manager{connections: make(map[string]*sql.DB)}
	for _, branch := range branches {
		dbFile := filepath.Join(absDir, fmt.Sprintf("aports-%s.db", branch))
		
		// Verify file exists
		if _, err := os.Stat(dbFile); os.IsNotExist(err) {
			log.Printf("⚠️  Database file not found: %s (Will be created empty on first write)", dbFile)
		} else {
			log.Printf("✅ Found database file: %s", dbFile)
		}

		conn, err := sql.Open("sqlite", dbFile)
		if err != nil {
			return nil, fmt.Errorf("failed to open %s: %w", dbFile, err)
		}

		// Immediately test the connection to catch permission/path errors early
		if err := conn.Ping(); err != nil {
			log.Printf("❌ Failed to ping database %s: %v", dbFile, err)
			log.Println("   --> Ensure no other process (like apkbrowser-updater) is locking this file.")
		}

		// Optimize SQLite for read-heavy web workloads
		conn.Exec("PRAGMA journal_mode=WAL;")
		conn.Exec("PRAGMA cache_size=-20000;")
		m.connections[branch] = conn
	}
	return m, nil
}

func (m *Manager) Get(branch string) *sql.DB {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connections[branch]
}

// Close closes all open database connections gracefully.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, db := range m.connections {
		db.Close()
	}
}

// CreateTables initializes the SQLite schema based on the Python reference.
func CreateTables(db *sql.DB) {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS 'packages' (
			'id' INTEGER PRIMARY KEY,
			'name' TEXT,
			'version' TEXT,
			'description' TEXT,
			'url' TEXT,
			'license' TEXT,
			'arch' TEXT,
			'repo' TEXT,
			'checksum' TEXT,
			'size' INTEGER,
			'installed_size' INTEGER,
			'origin' TEXT,
			'maintainer' INTEGER,
			'build_time' INTEGER,
			"commit" TEXT,
			'provider_priority' INTEGER,
			'fid' INTEGER
		)`,
		`CREATE INDEX IF NOT EXISTS 'packages_name' on 'packages' (name)`,
		`CREATE INDEX IF NOT EXISTS 'packages_maintainer' on 'packages' (maintainer)`,
		`CREATE INDEX IF NOT EXISTS 'packages_build_time' on 'packages' (build_time)`,
		`CREATE INDEX IF NOT EXISTS 'packages_origin' on 'packages' (origin)`,
		
		`CREATE TABLE IF NOT EXISTS 'files' (
			'id' INTEGER PRIMARY KEY,
			'file' TEXT,
			'path' TEXT,
			'pid' INTEGER REFERENCES packages(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS 'files_file' on 'files' (file)`,
		`CREATE INDEX IF NOT EXISTS 'files_path' on 'files' (path)`,
		`CREATE INDEX IF NOT EXISTS 'files_pid' on 'files' (pid)`,

		`CREATE TABLE IF NOT EXISTS maintainer (
			'id' INTEGER PRIMARY KEY,
			'name' TEXT,
			'email' TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS 'maintainer_name' on maintainer (name)`,

		`CREATE TABLE IF NOT EXISTS 'repoversion' (
			'repo' TEXT,
			'arch' TEXT,
			'version' TEXT,
			PRIMARY KEY ('repo', 'arch')
		) WITHOUT ROWID`,

		`CREATE TABLE IF NOT EXISTS 'flagged' (
			'origin' TEXT,
			'version' TEXT,
			'repo' TEXT,
			'created' INTEGER,
			'updated' INTEGER,
			'reporter' TEXT,
			'new_version' TEXT,
			'message' TEXT,
			PRIMARY KEY ('origin', 'version', 'repo')
		) WITHOUT ROWID`,
	}

	fields := []string{"provides", "depends", "install_if"}
	for _, field := range fields {
		schema = append(schema, fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS '%s' (
				'name' TEXT,
				'version' TEXT,
				'operator' TEXT,
				'pid' INTEGER REFERENCES packages(id) ON DELETE CASCADE
			)`, field))
		schema = append(schema, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS '%s_name' on %s (name)`, field, field))
		schema = append(schema, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS '%s_pid' on %s (pid)`, field, field))
	}

	for _, sqlStr := range schema {
		_, err := db.Exec(sqlStr)
		if err != nil {
			log.Printf("⚠️  Schema execution warning: %v", err)
		}
	}
}

// EnsureMaintainerExists inserts a maintainer if they don't exist and returns their ID.
func EnsureMaintainerExists(tx *sql.Tx, maintainer string) sql.NullInt64 {
	name := maintainer
	email := ""
	
	if idx := strings.Index(maintainer, "<"); idx != -1 {
		name = strings.TrimSpace(maintainer[:idx])
		email = strings.Trim(maintainer[idx:], "<> ")
	} else {
		return sql.NullInt64{Valid: false}
	}

	if email == "" {
		return sql.NullInt64{Valid: false}
	}

	sqlStr := `
		INSERT OR REPLACE INTO maintainer ('id', 'name', 'email')
		VALUES (
			(SELECT id FROM maintainer WHERE name=? and email=?),
			?, ?
		)
	`
	_, err := tx.Exec(sqlStr, name, email, name, email)
	if err != nil {
		return sql.NullInt64{Valid: false}
	}
	
	var selID int64
	err = tx.QueryRow("SELECT id FROM maintainer WHERE name=? and email=?", name, email).Scan(&selID)
	if err != nil {
		return sql.NullInt64{Valid: false}
	}
	return sql.NullInt64{Int64: selID, Valid: true}
}
