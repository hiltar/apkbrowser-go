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
	"path/filepath"
	"strings"
	"sync"

	"apkbrowser/internal/config"
	"apkbrowser/internal/database"

	_ "modernc.org/sqlite" // Pure Go SQLite driver
)

// Run starts the concurrent database update process for a specific branch.
func Run(cfg *config.Config, branch string, force bool) {
	dbPath := filepath.Join(cfg.Database.Path, fmt.Sprintf("aports-%s.db", branch))
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

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
			if key == "D" || key == "p" || key == "i" {
				buffer[key] = strings.Split(value, " ")
			} else {
				buffer[key] = value
			}
		}
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("Transaction begin error: %v", err)
		return
	}
	defer tx.Rollback()

	// Clear old data for this repo/arch
	tx.Exec("DELETE FROM files WHERE pid IN (SELECT id FROM packages WHERE repo = ? AND arch = ?)", repo, arch)
	tx.Exec("DELETE FROM depends WHERE pid IN (SELECT id FROM packages WHERE repo = ? AND arch = ?)", repo, arch)
	tx.Exec("DELETE FROM provides WHERE pid IN (SELECT id FROM packages WHERE repo = ? AND arch = ?)", repo, arch)
	tx.Exec("DELETE FROM install_if WHERE pid IN (SELECT id FROM packages WHERE repo = ? AND arch = ?)", repo, arch)
	tx.Exec("DELETE FROM packages WHERE repo = ? AND arch = ?", repo, arch)

	stmtPkg, _ := tx.Prepare(`INSERT INTO packages (name, version, description, url, license, arch, repo, checksum, size, installed_size, origin, maintainer, build_time, commit, provider_priority) 
	                          VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	stmtFile, _ := tx.Prepare(`INSERT INTO files (file, path, pid) VALUES (?, ?, ?)`)
	stmtDep, _ := tx.Prepare(`INSERT INTO depends (name, version, operator, pid) VALUES (?, ?, ?, ?)`)
	stmtProv, _ := tx.Prepare(`INSERT INTO provides (name, version, operator, pid) VALUES (?, ?, ?, ?)`)
	stmtIif, _ := tx.Prepare(`INSERT INTO install_if (name, version, operator, pid) VALUES (?, ?, ?, ?)`)

	for _, pkg := range packages {
		name, _ := pkg["P"].(string)
		ver, _ := pkg["V"].(string)
		
		var maintainerID sql.NullInt64
		if m, ok := pkg["m"].(string); ok && m != "" {
			maintainerID = database.EnsureMaintainerExists(tx, m)
		}

		res, err := stmtPkg.Exec(
			name, ver, pkg["T"], pkg["U"], pkg["L"], pkg["A"], repo, pkg["C"], 
			pkg["S"], pkg["I"], pkg["o"], maintainerID, pkg["t"], pkg["c"], pkg["k"],
		)
		if err != nil {
			continue
		}
		pid, _ := res.LastInsertId()

		// Insert Dependencies, Provides, Install-If
		insertRelations(stmtDep, pkg["D"], pid)
		insertRelations(stmtProv, pkg["p"], pid)
		insertRelations(stmtIif, pkg["i"], pid)

		// Fetch and insert file list from the actual .apk file
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

	tx.Exec("INSERT OR REPLACE INTO repoversion (version, repo, arch) VALUES (?, ?, ?)", version, repo, arch)
	tx.Commit()
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
