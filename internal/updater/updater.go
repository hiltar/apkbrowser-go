package updater

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"apkbrowser/internal/config"
	"apkbrowser/internal/database"

	_ "modernc.org/sqlite" // Pure Go SQLite driver
)

// Run starts the concurrent database update process for a specific branch.
func Run(cfg *config.Config, branch string, force bool) {
	// Automatically create the database directory if it doesn't exist
	if err := os.MkdirAll(cfg.Database.Path, 0755); err != nil {
		log.Fatalf("Failed to create database directory %s: %v", cfg.Database.Path, err)
	}

	dbPath := filepath.Join(cfg.Database.Path, fmt.Sprintf("aports-%s.db", branch))
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// SQLite only supports one concurrent writer. 
	// This forces Go to queue the goroutines safely, preventing "database is locked" errors.
	db.SetMaxOpenConns(1)

	// Optimize SQLite for bulk inserts
	db.Exec("PRAGMA synchronous = OFF;")
	db.Exec("PRAGMA journal_mode = MEMORY;")

	database.CreateTables(db)

	var wg sync.WaitGroup
	repos := cfg.GetRepos()
	arches := cfg.GetArches()

	log.Printf("Starting update for branch %s across %d repos and %d arches", branch, len(repos), len(arches))

	for _, repo := range repos {
		for _, arch := range arches {
			wg.Add(1)
			go func(r, a string) {
				defer wg.Done()
				processRepo(db, cfg, branch, r, a, force)
			}(repo, arch)
		}
	}
	wg.Wait()

	// Reset to safe defaults for web serving
	db.Exec("PRAGMA synchronous = NORMAL;")
	db.Exec("PRAGMA journal_mode = WAL;")
	log.Println("Database update complete.")
}

func processRepo(db *sql.DB, cfg *config.Config, branch, repo, arch string, force bool) {
	url := fmt.Sprintf("%s/%s/%s/%s/APKINDEX.tar.gz", cfg.Repository.URL, branch, repo, arch)
	log.Printf("[%s/%s] Fetching %s", repo, arch, url)

	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != 200 {
		log.Printf("[%s/%s] Skipping: HTTP %d / %v", repo, arch, resp.StatusCode, err)
		return
	}
	defer resp.Body.Close()

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		log.Printf("[%s/%s] Failed to create gzip reader: %v", repo, arch, err)
		return
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var indexData []byte
	var version string

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("[%s/%s] Tar read error: %v", repo, arch, err)
			return
		}

		if hdr.Name == "DESCRIPTION" {
			data, _ := io.ReadAll(tr)
			version = strings.TrimSpace(string(data))
		} else if hdr.Name == "APKINDEX" {
			indexData, _ = io.ReadAll(tr)
		}
	}

	if !force {
		var localVersion string
		row := db.QueryRow("SELECT version FROM repoversion WHERE repo = ? AND arch = ?", repo, arch)
		row.Scan(&localVersion)
		if version == localVersion {
			log.Printf("[%s/%s] Already up to date (v%s)", repo, arch, version)
			return
		}
	}

	log.Printf("[%s/%s] Parsing APKINDEX (v%s)", repo, arch, version)
	parseAndInsert(db, cfg, branch, repo, arch, indexData, version)
}

func parseAndInsert(db *sql.DB, cfg *config.Config, branch, repo, arch string, data []byte, version string) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	
	// Increase buffer size just in case of unusually long lines in APKINDEX
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	buffer := make(map[string]interface{})
	var packages []map[string]interface{}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			if len(buffer) > 0 {
				packages = append(packages, buffer)
				buffer = make(map[string]interface{})
			}
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			key := parts[0]
			value := parts[1]
			// Depends, Provides and Install-if are multi-value fields
			if key == "D" || key == "p" || key == "i" {
				buffer[key] = strings.Split(value, " ")
			} else {
				buffer[key] = value
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("[%s/%s] Scanner error: %v", repo, arch, err)
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("[%s/%s] Transaction begin error: %v", repo, arch, err)
		return
	}
	defer tx.Rollback()

	// Clear old data for this repo/arch before inserting new data
	tx.Exec("DELETE FROM files WHERE pid IN (SELECT id FROM packages WHERE repo = ? AND arch = ?)", repo, arch)
	tx.Exec("DELETE FROM depends WHERE pid IN (SELECT id FROM packages WHERE repo = ? AND arch = ?)", repo, arch)
	tx.Exec("DELETE FROM provides WHERE pid IN (SELECT id FROM packages WHERE repo = ? AND arch = ?)", repo, arch)
	tx.Exec("DELETE FROM install_if WHERE pid IN (SELECT id FROM packages WHERE repo = ? AND arch = ?)", repo, arch)
	tx.Exec("DELETE FROM packages WHERE repo = ? AND arch = ?", repo, arch)

	// Prepare statements with strict error checking to prevent nil pointer panics
        	stmtPkg, err := tx.Prepare(`INSERT INTO packages (name, version, description, url, license, arch, repo, checksum, size, installed_size, origin, maintainer, build_time, "commit", provider_priority) 
	                          VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)

	if err != nil {
		log.Printf("[%s/%s] Failed to prepare stmtPkg: %v", repo, arch, err)
		return
	}
	defer stmtPkg.Close()

	stmtFile, err := tx.Prepare(`INSERT INTO files (file, path, pid) VALUES (?, ?, ?)`)
	if err != nil {
		log.Printf("[%s/%s] Failed to prepare stmtFile: %v", repo, arch, err)
		return
	}
	defer stmtFile.Close()

	stmtDep, err := tx.Prepare(`INSERT INTO depends (name, version, operator, pid) VALUES (?, ?, ?, ?)`)
	if err != nil {
		log.Printf("[%s/%s] Failed to prepare stmtDep: %v", repo, arch, err)
		return
	}
	defer stmtDep.Close()

	stmtProv, err := tx.Prepare(`INSERT INTO provides (name, version, operator, pid) VALUES (?, ?, ?, ?)`)
	if err != nil {
		log.Printf("[%s/%s] Failed to prepare stmtProv: %v", repo, arch, err)
		return
	}
	defer stmtProv.Close()

	stmtIif, err := tx.Prepare(`INSERT INTO install_if (name, version, operator, pid) VALUES (?, ?, ?, ?)`)
	if err != nil {
		log.Printf("[%s/%s] Failed to prepare stmtIif: %v", repo, arch, err)
		return
	}
	defer stmtIif.Close()

	for _, pkg := range packages {
		var maintainerID sql.NullInt64
		if m, ok := pkg["m"].(string); ok && m != "" {
			maintainerID = database.EnsureMaintainerExists(tx, m)
		}

		// Ensure 'k' (provider_priority) is nil if missing, matching Python's behavior
		kVal := pkg["k"]
		if kVal == "" {
			kVal = nil
		}

		res, err := stmtPkg.Exec(
			pkg["P"], pkg["V"], pkg["T"], pkg["U"], pkg["L"], arch, repo, pkg["C"],
			pkg["S"], pkg["I"], pkg["o"], maintainerID, pkg["t"], pkg["c"], kVal,
		)
		if err != nil {
			log.Printf("[%s/%s] Failed to insert package %s: %v", repo, arch, pkg["P"], err)
			continue
		}
		pid, _ := res.LastInsertId()

		// Insert Dependencies, Provides, Install-If
		insertRelations(stmtDep, pkg["D"], pid)
		insertRelations(stmtProv, pkg["p"], pid)
		insertRelations(stmtIif, pkg["i"], pid)

		// Fetch and insert file list from the actual .apk file
		name, _ := pkg["P"].(string)
		ver, _ := pkg["V"].(string)
		apkURL := fmt.Sprintf("%s/%s/%s/%s/%s-%s.apk", cfg.Repository.URL, branch, repo, arch, name, ver)
		
		files, err := getFileList(apkURL)
		if err == nil {
			for _, f := range files {
				fname := filepath.Base(f)
				fpath := filepath.Dir(f)
				stmtFile.Exec(fname, fpath, pid)
			}
		}
	}

	_, err = tx.Exec("INSERT OR REPLACE INTO repoversion (version, repo, arch) VALUES (?, ?, ?)", version, repo, arch)
	if err != nil {
		log.Printf("[%s/%s] Failed to update repoversion: %v", repo, arch, err)
	}

	err = tx.Commit()
	if err != nil {
		log.Printf("[%s/%s] Transaction commit error: %v", repo, arch, err)
		return
	}
	log.Printf("[%s/%s] Successfully inserted %d packages", repo, arch, len(packages))
}

func insertRelations(stmt *sql.Stmt, data interface{}, pid int64) {
	if items, ok := data.([]string); ok {
		for _, item := range items {
			name, op, ver := parseVersionOperator(item)
			stmt.Exec(name, ver, op, pid)
		}
	}
}

func parseVersionOperator(pkg string) (string, string, string) {
	operators := []string{">=", "<=", "><", "=", ">", "<"}
	for _, op := range operators {
		if strings.Contains(pkg, op) {
			parts := strings.SplitN(pkg, op, 2)
			if len(parts) == 2 {
				return parts[0], op, parts[1]
			}
		}
	}
	return pkg, "", ""
}

// getFileList streams the .apk file (handling concatenated gzip streams) to extract file paths.
func getFileList(url string) ([]string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var files []string
	// Alpine .apk files are often concatenated gzip streams. We must loop until EOF.
	for {
		gz, err := gzip.NewReader(resp.Body)
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, err
		}

		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				gz.Close()
				return files, err
			}
			if strings.HasPrefix(hdr.Name, ".") || hdr.Typeflag == tar.TypeDir {
				continue
			}
			files = append(files, "/"+hdr.Name)
		}
		gz.Close()
	}
	return files, nil
}
