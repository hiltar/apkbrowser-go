package handlers

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"apkbrowser/internal/config"
	"apkbrowser/internal/database"

	"github.com/go-chi/chi/v5"
)

type Handler struct {
	Config *config.Config
	DB     *database.Manager
	Repos  map[string]*database.Repository
	Tmpl   *template.Template
	mu     sync.RWMutex
}

// Repo lazily initializes and caches the Repository for a specific branch.
func (h *Handler) Repo(branch string) *database.Repository {
	h.mu.RLock()
	if r, ok := h.Repos[branch]; ok {
		h.mu.RUnlock()
		return r
	}
	h.mu.RUnlock()

	h.mu.Lock()
	defer h.mu.Unlock()

	// Double-check after acquiring write lock
	if r, ok := h.Repos[branch]; ok {
		return r
	}

	r := database.NewRepository(h.DB.Get(branch))
	h.Repos[branch] = r
	return r
}

func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/packages", http.StatusSeeOther)
	})
	r.Get("/packages", h.PackagesPage)
	r.Get("/contents", h.ContentsPage)
	r.Get("/package/{branch}/{repo}/{arch}/{name}", h.PackageDetails)
}

// --- Helper Functions ---

func sizeofFmt(num interface{}) string {
	var n float64
	switch v := num.(type) {
	case int64:
		n = float64(v)
	case int:
		n = float64(v)
	case float64:
		n = v
	default:
		return "0B"
	}

	units := []string{"", "Ki", "Mi", "Gi", "Ti", "Pi", "Ei", "Zi"}
	for _, unit := range units {
		if math.Abs(n) < 1024.0 {
			return fmt.Sprintf("%3.1f%sB", n, unit)
		}
		n /= 1024.0
	}
	return fmt.Sprintf("%.1fYiB", n)
}

func formatURL(tmpl string, vars map[string]string) string {
	res := tmpl
	for k, v := range vars {
		res = strings.ReplaceAll(res, "{"+k+"}", v)
	}
	return res
}

func getPagination(page, pages int) (int, int) {
	start := page - 4
	if start < 1 {
		start = 1
	}
	end := page + 3
	if end > pages {
		end = pages
	}
	return start, end
}

// --- HTTP Handlers ---

func (h *Handler) PackagesPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("name")
	branch := q.Get("branch")
	repo := q.Get("repo")
	arch := q.Get("arch")
	maintainer := q.Get("maintainer")
	origin := q.Get("origin")
	pageStr := q.Get("page")

	if branch == "" {
		branch = h.Config.Repository.DefaultBranch
	}

	page := 1
	if pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
			page = p
		}
	}

	form := map[string]interface{}{
		"name": name, "branch": branch, "repo": repo,
		"arch": arch, "maintainer": maintainer, "origin": origin, "page": page,
	}

	f := database.Filter{
		Name: name, Arch: arch, Repo: repo,
		Maintainer: maintainer, Origin: origin,
	}

	rep := h.Repo(branch)
	fr := rep.BuildFilter(f)

	numPackages, _ := rep.GetNumPackages(fr)
	pages := int(math.Ceil(float64(numPackages) / 50.0))

	offset := (page - 1) * 50
	packages, _ := rep.GetPackages(offset, fr)
	maintainers, _ := rep.GetMaintainers()

	pagStart, pagStop := getPagination(page, pages)

	data := map[string]interface{}{
		"distro_name": h.Config.Branding.Name, "logo": h.Config.Branding.Logo,
		"favicon": h.Config.Branding.Favicon, "flagging": h.Config.Settings.Flagging == "yes",
		"external_wiki": h.Config.External.Wiki, "external_mirrors": h.Config.External.Mirrors,
		"title": "Package index", "form": form, "branches": h.Config.GetBranches(),
		"arches": h.Config.GetArches(), "repos": h.Config.GetRepos(),
		"maintainers": maintainers, "packages": packages,
		"pag_start": pagStart, "pag_stop": pagStop, "pages": pages,
	}

	h.Tmpl.ExecuteTemplate(w, "index.html", data)
}

func (h *Handler) ContentsPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	file := q.Get("file")
	path := q.Get("path")
	name := q.Get("name")
	branch := q.Get("branch")
	repo := q.Get("repo")
	arch := q.Get("arch")
	pageStr := q.Get("page")

	if branch == "" {
		branch = h.Config.Repository.DefaultBranch
	}
	if repo == "" {
		repo = h.Config.Repository.DefaultRepo
	}

	page := 1
	if pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
			page = p
		}
	}

	form := map[string]interface{}{
		"file": file, "path": path, "name": name, "branch": branch,
		"repo": repo, "arch": arch, "page": page,
	}

	var contents []map[string]interface{}
	var numContents int

	// Prevent full table scan on contents page if no search criteria is provided
	if name == "" && file == "" && path == "" {
		contents = []map[string]interface{}{}
		numContents = 0
	} else {
		f := database.Filter{
			Name: name, Arch: arch, Repo: repo,
			File: file, Path: path,
		}
		rep := h.Repo(branch)
		fr := rep.BuildFilter(f)
		
		numContents, _ = rep.GetNumContents(fr)
		offset := (page - 1) * 50
		contents, _ = rep.GetContents(offset, fr)
	}

	pages := int(math.Ceil(float64(numContents) / 50.0))
	pagStart, pagStop := getPagination(page, pages)

	data := map[string]interface{}{
		"distro_name": h.Config.Branding.Name, "logo": h.Config.Branding.Logo,
		"favicon": h.Config.Branding.Favicon, "flagging": h.Config.Settings.Flagging == "yes",
		"external_wiki": h.Config.External.Wiki, "external_mirrors": h.Config.External.Mirrors,
		"title": "Contents index", "form": form, "branches": h.Config.GetBranches(),
		"arches": h.Config.GetArches(), "repos": h.Config.GetRepos(),
		"contents": contents, "pag_start": pagStart, "pag_stop": pagStop, "pages": pages,
	}

	h.Tmpl.ExecuteTemplate(w, "contents.html", data)
}

func (h *Handler) PackageDetails(w http.ResponseWriter, r *http.Request) {
	branch := chi.URLParam(r, "branch")
	repo := chi.URLParam(r, "repo")
	arch := chi.URLParam(r, "arch")
	name := chi.URLParam(r, "name")

	rep := h.Repo(branch)
	pkg, err := rep.GetPackage(repo, arch, name)
	if err != nil || pkg == nil {
		http.Error(w, "Not Found", 404)
		return
	}

	pkg["size_fmt"] = sizeofFmt(pkg["size"])
	pkg["installed_size_fmt"] = sizeofFmt(pkg["installed_size"])

	gitCommit := ""
	if c, ok := pkg["commit"].(string); ok {
		gitCommit = strings.Replace(c, "-dirty", "", -1)
	}

	getStr := func(key string) string {
		if v, ok := pkg[key].(string); ok {
			return v
		}
		return ""
	}

	vars := map[string]string{
		"commit": gitCommit, "branch": branch, "repo": repo, "arch": arch,
		"name": name, "version": getStr("version"), "origin": getStr("origin"),
	}

	gitURL := formatURL(h.Config.External.GitCommit, vars)
	repoURL := formatURL(h.Config.External.GitRepo, vars)
	buildURL := formatURL(h.Config.External.BuildLog, vars)

	var pkgID int64
	if id, ok := pkg["id"].(int64); ok {
		pkgID = id
	}

	depends, _ := rep.GetDepends(pkgID, arch)
	requiredBy, _ := rep.GetRequiredBy(pkgID, arch)
	subpackages, _ := rep.GetSubpackages(repo, pkg["origin"], arch)
	installIf, _ := rep.GetInstallIf(pkgID)
	provides, _ := rep.GetProvides(pkgID, name)

	data := map[string]interface{}{
		"distro_name": h.Config.Branding.Name, "logo": h.Config.Branding.Logo,
		"favicon": h.Config.Branding.Favicon, "flagging": h.Config.Settings.Flagging == "yes",
		"external_wiki": h.Config.External.Wiki, "external_mirrors": h.Config.External.Mirrors,
		"title": name, "branch": branch, "git_url": gitURL,
		"repo_url": repoURL, "build_log_url": buildURL,
		"num_depends": len(depends), "depends": depends,
		"num_required_by": len(requiredBy), "required_by": requiredBy,
		"num_subpackages": len(subpackages), "subpackages": subpackages,
		"install_if": installIf, "num_install_if": len(installIf),
		"provides": provides, "num_provides": len(provides), "pkg": pkg,
	}

	h.Tmpl.ExecuteTemplate(w, "package.html", data)
}
