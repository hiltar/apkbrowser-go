package main

import (
	"apkbrowser/internal/config"
	"apkbrowser/internal/database"
	"apkbrowser/internal/handlers"
	"html/template"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func main() {
	cfg, err := config.Load("config.ini")
	if err != nil {
		log.Fatal(err)
	}

	dbMgr, err := database.Open(cfg.Database.Path, cfg.GetBranches())
	if err != nil {
		log.Fatal(err)
	}
	defer dbMgr.Close()

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

	tmpl := template.Must(template.New("").Funcs(funcMap).ParseGlob("templates/*.html"))

	h := &handlers.Handler{
		Config: cfg,
		DB:     dbMgr,
		Repos:  make(map[string]*database.Repository),
		Tmpl:   tmpl,
	}

	r := chi.NewRouter()

	fileServer := http.FileServer(http.Dir("./static"))
	r.Handle("/static/*", http.StripPrefix("/static/", fileServer))

	h.RegisterRoutes(r)

	log.Println("Starting server on :8080")
	http.ListenAndServe(":8080", r)
}
