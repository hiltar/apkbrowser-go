# APK Browser (Go)

A high-performance, statically compiled web application for browsing postmarketOS and Alpine Linux APK package repositories.

## 📖 Overview
This project is a complete rewrite of the original [postmarketOS/apkbrowser](https://gitlab.postmarketos.org/postmarketOS/apkbrowser) (Python/Flask) in **Go**. It provides a fast, lightweight interface to search packages, inspect file contents, view dependencies, and track package metadata across multiple architectures and branches.

## 🚀 Why Go?
The original Python implementation works well but has limitations in concurrency, memory usage during database generation, and deployment complexity. This Go rewrite was built to address those pain points:
- **Blazing Fast DB Generation:** Concurrently fetches and parses `APKINDEX.tar.gz` files using Go's goroutines.
- **Zero Memory Bloat:** Streams remote `.apk` and index files directly through gzip/tar readers instead of loading them entirely into RAM.
- **Single Binary Deployment:** Templates, CSS, and static assets are compiled directly into the executable. No virtual environments, `pip`, or `uwsgi` required.
- **Instant Search:** Leverages SQLite FTS5 for sub-millisecond package and file lookups.
- **True Cross-Platform:** Compiles to a single static binary for Linux (ARM64/AMD64), macOS, or Windows without CGO dependencies.

## ✨ Features
- 🔍 Full-text search across package names, descriptions, and file paths
- 📦 Dependency & reverse-dependency resolution
- 📁 File contents browser with wildcard/GLOB support
- 🌍 Multi-branch, multi-architecture, and multi-repo support
- 📊 Modern, responsive PureCSS-based UI
- ⚡ Automatic FTS5 index synchronization
- 🔗 External links to Git commits, repositories, and build logs

## 🛠️ Getting Started

### Prerequisites
- Go 1.21+
- `config.ini` (see configuration section below)

### Build
```bash
# Initialize module & download dependencies
go mod init apkbrowser
go mod tidy

# Build both binaries (statically linked, no CGO)
CGO_ENABLED=0 go build -o ./server ./cmd/server
CGO_ENABLED=0 go build -o ./updater ./cmd/updater
```

### Configuration
```
[branding]
name = postmarketOS
logo = logo.png
favicon = favicon.ico

[repository]
url = https://mirror.postmarketos.org/postmarketos
branches = master,edge
default-branch = master
repos = postmarketos,main
default-repo = postmarketos
arches = x86_64,aarch64,armv7,armhf,x86

[database]
path = db

[external]
wiki = https://wiki.postmarketos.org
mirrors = https://postmarketos.org/mirrors
git-commit = https://gitlab.postmarketos.org/postmarketOS/pmaports/-/commit/{commit}
git-repo = https://gitlab.postmarketos.org/postmarketOS/pmaports/-/tree/{branch}/main/{name}
build-log = https://build.postmarketos.org/?branch={branch}&arch={arch}&job={name}-{version}
```

### Generate database
```
./updater -config config.ini -branch master
```  
(Note: The first run will download and parse repository indexes. This may take a few minutes depending on your network.)  

### Run server
```
./server
```
