package database

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// Filter holds the search parameters for querying packages and files.
type Filter struct {
	Name       string
	Arch       string
	Repo       string
	Maintainer string
	Origin     string
	File       string
	Path       string
}

// FilterResult contains the generated SQL WHERE clause and its arguments.
type FilterResult struct {
	WhereClause  string
	Args         []interface{}
	JoinProvides bool
}

// Repository handles all database interactions for a specific branch.
type Repository struct {
	db *sql.DB
}

// NewRepository creates a new Repository instance and automatically ensures 
// the FTS5 search indexes are created and populated.
func NewRepository(db *sql.DB) *Repository {
	r := &Repository{db: db}
	
	// Automatically set up and sync FTS5 search indexes under the hood
	if err := r.ensureFTS5(); err != nil {
		log.Printf("Warning: Failed to initialize FTS5 search index: %v", err)
	}
	
	return r
}

// ensureFTS5 creates the virtual tables, sync triggers, and populates existing data.
func (r *Repository) ensureFTS5() error {
	// Ensure base tables exist
	CreateTables(r.db)

	// 1. Create FTS5 virtual tables
	_, err := r.db.Exec(`
		CREATE VIRTUAL TABLE IF NOT EXISTS packages_fts USING fts5(name, description, content=packages, content_rowid=id);
		CREATE VIRTUAL TABLE IF NOT EXISTS files_fts USING fts5(file, path, content=files, content_rowid=id);
	`)
	if err != nil {
		log.Printf("⚠️  FTS5 tables failed to create (search will fallback to GLOB): %v", err)
		return nil // Graceful fallback, don't crash
	}

	// 2. Create triggers to keep packages_fts in sync automatically
	_, err = r.db.Exec(`
		CREATE TRIGGER IF NOT EXISTS packages_fts_ai AFTER INSERT ON packages BEGIN
		  INSERT INTO packages_fts(rowid, name, description) VALUES (new.id, new.name, new.description);
		END;
		CREATE TRIGGER IF NOT EXISTS packages_fts_ad AFTER DELETE ON packages BEGIN
		  INSERT INTO packages_fts(packages_fts, rowid, name, description) VALUES('delete', old.id, old.name, old.description);
		END;
		CREATE TRIGGER IF NOT EXISTS packages_fts_au AFTER UPDATE ON packages BEGIN
		  INSERT INTO packages_fts(packages_fts, rowid, name, description) VALUES('delete', old.id, old.name, old.description);
		  INSERT INTO packages_fts(rowid, name, description) VALUES (new.id, new.name, new.description);
		END;
	`)
	if err != nil {
		log.Printf("⚠️  FTS5 packages triggers failed: %v", err)
	}

	// 3. Create triggers to keep files_fts in sync automatically
	_, err = r.db.Exec(`
		CREATE TRIGGER IF NOT EXISTS files_fts_ai AFTER INSERT ON files BEGIN
		  INSERT INTO files_fts(rowid, file, path) VALUES (new.id, new.file, new.path);
		END;
		CREATE TRIGGER IF NOT EXISTS files_fts_ad AFTER DELETE ON files BEGIN
		  INSERT INTO files_fts(files_fts, rowid, file, path) VALUES('delete', old.id, old.file, old.path);
		END;
		CREATE TRIGGER IF NOT EXISTS files_fts_au AFTER UPDATE ON files BEGIN
		  INSERT INTO files_fts(files_fts, rowid, file, path) VALUES('delete', old.id, old.file, old.path);
		  INSERT INTO files_fts(rowid, file, path) VALUES (new.id, new.file, new.path);
		END;
	`)
	if err != nil {
		log.Printf("⚠️  FTS5 files triggers failed: %v", err)
	}

	// 4. Rebuild the search index if empty
	var pkgCount, ftsPkgCount int
	r.db.QueryRow("SELECT count(*) FROM packages").Scan(&pkgCount)
	r.db.QueryRow("SELECT count(*) FROM packages_fts").Scan(&ftsPkgCount)
	if pkgCount > 0 && ftsPkgCount == 0 {
		log.Println("🔨 Populating packages FTS5 search index (first run may take a moment)...")
		r.db.Exec("INSERT INTO packages_fts(packages_fts) VALUES('rebuild')")
	}

	var fileCount, ftsFileCount int
	r.db.QueryRow("SELECT count(*) FROM files").Scan(&fileCount)
	r.db.QueryRow("SELECT count(*) FROM files_fts").Scan(&ftsFileCount)
	if fileCount > 0 && ftsFileCount == 0 {
		log.Println("🔨 Populating files FTS5 search index (first run may take a moment)...")
		r.db.Exec("INSERT INTO files_fts(files_fts) VALUES('rebuild')")
	}

	return nil
}

// formatFTSQuery cleans user input to prevent FTS5 syntax errors.
func formatFTSQuery(q string) string {
	// Strip leading/trailing wildcards as FTS5 doesn't support leading wildcards
	q = strings.Trim(q, "* ")
	if q == "" {
		return ""
	}
	// Escape double quotes
	q = strings.ReplaceAll(q, "\"", "\"\"")
	return q
}

// BuildFilter dynamically constructs the SQL WHERE clause based on the provided Filter.
func (r *Repository) BuildFilter(f Filter) FilterResult {
	type filterField struct {
		Key   string
		Value string
	}

	fields := []filterField{
		{"packages.name", f.Name},
		{"packages.arch", f.Arch},
		{"packages.repo", f.Repo},
		{"maintainer.name", f.Maintainer},
		{"files.file", f.File},
		{"files.path", f.Path},
	}

	globFields := map[string]bool{
		"packages.name": true,
		"files.file":    true,
		"files.path":    true,
	}

	var where []string
	var args []interface{}
	joinProvides := false

	for _, field := range fields {
		if field.Value == "" {
			continue
		}
		
		isWildcard := strings.HasPrefix(field.Value, "*") || strings.HasSuffix(field.Value, "*")

		if field.Key == "packages.name" && strings.Contains(field.Value, ":") {
			where = append(where, "provides.name = ?")
			joinProvides = true
			args = append(args, field.Value)
		} else if globFields[field.Key] && isWildcard {
			// User explicitly used wildcards (e.g. *ssl*), fallback to GLOB for exact substring matching
			where = append(where, field.Key+" GLOB ?")
			args = append(args, field.Value)
		} else if globFields[field.Key] {
			// Plain text search, use FTS5 for blazing fast word matching
			ftsQuery := formatFTSQuery(field.Value)
			if ftsQuery == "" {
				where = append(where, field.Key+" GLOB ?")
				args = append(args, field.Value)
			} else {
				mainTable := strings.Split(field.Key, ".")[0]
				colName := strings.Split(field.Key, ".")[1]
				ftsTable := mainTable + "_fts"
				
				where = append(where, fmt.Sprintf("%s.rowid IN (SELECT rowid FROM %s WHERE %s MATCH ?)", mainTable, ftsTable, colName))
				args = append(args, ftsQuery)
			}
		} else {
			where = append(where, field.Key+" = ?")
			args = append(args, field.Value)
		}
	}

	if f.Origin != "" {
		where = append(where, "packages.origin = packages.name")
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	return FilterResult{
		WhereClause:  whereClause,
		Args:         args,
		JoinProvides: joinProvides,
	}
}

// scanToMap dynamically scans SQL rows into a slice of maps.
func scanToMap(rows *sql.Rows) ([]map[string]interface{}, error) {
	if rows == nil {
		return nil, nil
	}
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var results []map[string]interface{}

	for rows.Next() {
		columns := make([]interface{}, len(cols))
		columnPointers := make([]interface{}, len(cols))
		for i := range columns {
			columnPointers[i] = &columns[i]
		}

		if err := rows.Scan(columnPointers...); err != nil {
			return nil, err
		}

		pkg := make(map[string]interface{})
		for i, colName := range cols {
			val := columnPointers[i].(*interface{})
			pkg[colName] = *val
		}
		results = append(results, pkg)
	}
	return results, rows.Err()
}

// toInt safely converts an interface{} to an int.
func toInt(val interface{}) int {
	if val == nil {
		return -1
	}
	switch v := val.(type) {
	case int64:
		return int(v)
	case int:
		return v
	case float64:
		return int(v)
	case []uint8:
		if i, err := strconv.Atoi(string(v)); err == nil {
			return i
		}
	}
	return -1
}

// --- Query Methods ---

func (r *Repository) GetMaintainers() ([]string, error) {
	rows, err := r.db.Query("SELECT name FROM maintainer ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var maintainers []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		maintainers = append(maintainers, m)
	}
	return maintainers, rows.Err()
}

func (r *Repository) GetNumPackages(fr FilterResult) (int, error) {
	pjoin := ""
	if fr.JoinProvides {
		pjoin = "LEFT JOIN provides ON provides.pid = packages.id"
	}

	sqlStr := fmt.Sprintf(`
		SELECT count(*) as qty
		FROM packages
		LEFT JOIN maintainer ON packages.maintainer = maintainer.id
		%s
		%s
	`, pjoin, fr.WhereClause)

	var qty int
	err := r.db.QueryRow(sqlStr, fr.Args...).Scan(&qty)
	return qty, err
}

func (r *Repository) GetPackages(offset int, fr FilterResult) ([]map[string]interface{}, error) {
	pjoin := ""
	if fr.JoinProvides {
		pjoin = "LEFT JOIN provides ON provides.pid = packages.id"
	}

	sqlStr := fmt.Sprintf(`
		SELECT packages.*, datetime(packages.build_time, 'unixepoch') as build_time_fmt,
		       maintainer.name as mname, maintainer.email as memail,
		       datetime(flagged.created, 'unixepoch') as flagged
		FROM packages
		LEFT JOIN maintainer ON packages.maintainer = maintainer.id
		LEFT JOIN flagged ON packages.origin = flagged.origin
		    AND packages.version = flagged.version AND packages.repo = flagged.repo
		%s
		%s
		ORDER BY packages.build_time DESC
		LIMIT 50 OFFSET ?
	`, pjoin, fr.WhereClause)

	args := append(fr.Args, offset)
	rows, err := r.db.Query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanToMap(rows)
}

func (r *Repository) GetNumContents(fr FilterResult) (int, error) {
	sqlStr := fmt.Sprintf(`
		SELECT count(packages.id)
		FROM packages
		JOIN files ON files.pid = packages.id
		%s
	`, fr.WhereClause)

	var qty int
	err := r.db.QueryRow(sqlStr, fr.Args...).Scan(&qty)
	return qty, err
}

func (r *Repository) GetContents(offset int, fr FilterResult) ([]map[string]interface{}, error) {
	sqlStr := fmt.Sprintf(`
		SELECT packages.repo, packages.arch, packages.name, files.*
		FROM packages
		JOIN files ON files.pid = packages.id
		%s
		ORDER BY files.path, files.file
		LIMIT 50 OFFSET ?
	`, fr.WhereClause)

	args := append(fr.Args, offset)
	rows, err := r.db.Query(sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanToMap(rows)
}

func (r *Repository) GetPackage(repo, arch, name string) (map[string]interface{}, error) {
	sqlStr := `
		SELECT packages.*, datetime(packages.build_time, 'unixepoch') as build_time_fmt,
		       maintainer.name as mname, maintainer.email as memail,
		       datetime(flagged.created, 'unixepoch') as flagged
		FROM packages
		LEFT JOIN maintainer ON packages.maintainer = maintainer.id
		LEFT JOIN flagged ON packages.origin = flagged.origin
		    AND packages.version = flagged.version AND packages.repo = flagged.repo
		WHERE packages.repo = ? AND packages.arch = ? AND packages.name = ?
	`
	rows, err := r.db.Query(sqlStr, repo, arch, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	res, err := scanToMap(rows)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, nil
	}
	return res[0], nil
}

func (r *Repository) GetDepends(packageID int64, arch string) ([]map[string]interface{}, error) {
	sqlProvides := `
		SELECT de.name, pa.repo, pa.arch, pa.name as pkg_name, pa.provider_priority, de.name as depname
		FROM depends de
		JOIN provides pr ON de.name = pr.name
		LEFT JOIN packages pa ON pr.pid = pa.id
		WHERE de.pid = ? AND pa.arch = ?
	`
	sqlDirect := `
		SELECT de.name, dp.repo, dp.arch, dp.name as pkg_name, dp.provider_priority, de.name as depname
		FROM depends de
		JOIN packages dp ON dp.name = de.name
		WHERE de.pid = ? AND dp.arch = ?
	`
	sqlNames := `SELECT de.name as depname FROM depends de WHERE de.pid = ?`

	rowsP, err := r.db.Query(sqlProvides, packageID, arch)
	if err != nil { return nil, err }
	throughProvides, _ := scanToMap(rowsP)
	rowsP.Close()
	
	providesMap := make(map[string]map[string]interface{})
	for _, p := range throughProvides {
		if name, ok := p["depname"].(string); ok {
			providesMap[name] = p
		}
	}

	rowsD, err := r.db.Query(sqlDirect, packageID, arch)
	if err != nil { return nil, err }
	directDependency, _ := scanToMap(rowsD)
	rowsD.Close()
	
	directMap := make(map[string]map[string]interface{})
	for _, p := range directDependency {
		if name, ok := p["depname"].(string); ok {
			directMap[name] = p
		}
	}

	rowsN, err := r.db.Query(sqlNames, packageID)
	if err != nil { return nil, err }
	allDeps, _ := scanToMap(rowsN)
	rowsN.Close()

	var result []map[string]interface{}
	for _, dep := range allDeps {
		name, _ := dep["depname"].(string)
		var finalDep map[string]interface{}
		prio := -1

		if d, ok := directMap[name]; ok {
			finalDep = d
			prio = toInt(d["provider_priority"])
		}

		if p, ok := providesMap[name]; ok {
			pPrio := toInt(p["provider_priority"])
			if pPrio > prio {
				finalDep = p
			}
		}

		if finalDep == nil {
			result = append(result, map[string]interface{}{"name": name})
		} else {
			result = append(result, finalDep)
		}
	}
	return result, nil
}

func (r *Repository) GetRequiredBy(packageID int64, arch string) ([]map[string]interface{}, error) {
	sqlStr := `
		SELECT DISTINCT packages.* FROM provides
		LEFT JOIN depends ON provides.name = depends.name
		LEFT JOIN packages ON depends.pid = packages.id
		WHERE packages.arch = ? AND provides.pid = ?
		ORDER BY packages.name
	`
	rows, err := r.db.Query(sqlStr, arch, packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanToMap(rows)
}

func (r *Repository) GetSubpackages(repo string, origin interface{}, arch string) ([]map[string]interface{}, error) {
	sqlStr := `
		SELECT DISTINCT packages.* FROM packages
		WHERE repo = ? AND arch = ? AND origin = ?
		ORDER BY packages.name
	`
	rows, err := r.db.Query(sqlStr, repo, arch, origin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanToMap(rows)
}

func (r *Repository) GetInstallIf(packageID int64) ([]map[string]interface{}, error) {
	sqlStr := `SELECT name, operator, version FROM install_if WHERE pid = ?`
	rows, err := r.db.Query(sqlStr, packageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanToMap(rows)
}

func (r *Repository) GetProvides(packageID int64, pkgname string) ([]map[string]interface{}, error) {
	sqlStr := `SELECT name, operator, version FROM provides WHERE pid = ? AND name != ?`
	rows, err := r.db.Query(sqlStr, packageID, pkgname)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanToMap(rows)
}
