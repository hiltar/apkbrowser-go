package main

import (
	"html/template"
	"log"
	"net/http"

	"apkbrowser/internal/config"
	"apkbrowser/internal/database"
	"apkbrowser/internal/handlers"
	"apkbrowser/static"
	"apkbrowser/templates"

	"github.com/go-chi/chi/v5"
)

func main() {
	// Note: config.ini and the database files are still read from the local filesystem.
	// This is intentional so you can edit configurations and persist data without recompiling.
	cfg, err := config.Load("config.ini")
	if err != nil {
		log.Fatal(err)
	}

	dbMgr, err := database.Open(cfg.Database.Path, cfg.GetBranches())
	if err != nil {
		log.Fatal(err)
	}
	defer dbMgr.Close()

	// 1. Register custom template functions for pagination math
	funcMap := template.FuncMap{
		"seq": func(start, end int) []int {
			var s []int
			for i := start; i <= end; i++ {
				s = append(s, i)
			}
			return s
		},
		"add": func(a, b int) int {
			return a + b
		},
	}

	// 2. Parse templates from the embedded templates package
	// Notice the pattern is just "*.html" because the FS is already rooted in the templates folder
	tmpl := template.Must(template.New("").Funcs(funcMap).ParseFS(templates.Files, "*.html"))

	h := &handlers.Handler{
		Config: cfg,
		DB:     dbMgr,
		Repos:  make(map[string]*database.Repository),
		Tmpl:   tmpl,
	}

	r := chi.NewRouter()

	// 3. Serve embedded static files
	// static.Files is rooted at the static directory, so it contains "css/style.css", etc.
	fileServer := http.FileServer(http.FS(static.Files))
	r.Handle("/static/*", http.StripPrefix("/static/", fileServer))

	h.RegisterRoutes(r)

	log.Println("Starting server on :8080")
	http.ListenAndServe(":8080", r)
}
