package database

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"

	_ "github.com/glebarez/go-sqlite"
)

type Manager struct {
	connections map[string]*sql.DB
	mu          sync.RWMutex
}

func Open(dbDir string, branches []string) (*Manager, error) {
	m := &Manager{connections: make(map[string]*sql.DB)}
	for _, branch := range branches {
		dbFile := filepath.Join(dbDir, fmt.Sprintf("aports-%s.db", branch))
		conn, err := sql.Open("sqlite", dbFile)
		if err != nil {
			return nil, err
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
