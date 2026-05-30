package main

import (
	"apkbrowser/internal/config"
	"apkbrowser/internal/updater"
	"flag"
	"log"
)

func main() {
	configPath := flag.String("config", "config.ini", "Path to config file")
	branch := flag.String("branch", "master", "Branch to update")
	force := flag.Bool("force", false, "Force update")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil { log.Fatal(err) }

	updater.Run(cfg, *branch, *force)
}
