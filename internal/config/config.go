package config

import (
	"strings"
	"gopkg.in/ini.v1"
)

type Config struct {
	Branding struct {
		Name    string `ini:"name"`
		Logo    string `ini:"logo"`
		Favicon string `ini:"favicon"`
	} `ini:"branding"`
	Settings struct {
		Flagging string `ini:"flagging"`
	} `ini:"settings"`
	External struct {
		Wiki      string `ini:"wiki"`
		Mirrors   string `ini:"mirrors"`
		GitCommit string `ini:"git-commit"`
		GitRepo   string `ini:"git-repo"`
		BuildLog  string `ini:"build-log"`
	} `ini:"external"`
	Repository struct {
		URL           string `ini:"url"`
		Branches      string `ini:"branches"`
		DefaultBranch string `ini:"default-branch"`
		Repos         string `ini:"repos"`
		DefaultRepo   string `ini:"default-repo"`
		Arches        string `ini:"arches"`
	} `ini:"repository"`
	Database struct {
		Path string `ini:"path"`
	} `ini:"database"`
}

func (c *Config) GetBranches() []string { return strings.Split(c.Repository.Branches, ",") }
func (c *Config) GetArches() []string   { return strings.Split(c.Repository.Arches, ",") }
func (c *Config) GetRepos() []string    { return strings.Split(c.Repository.Repos, ",") }

func Load(path string) (*Config, error) {
	cfg, err := ini.Load(path)
	if err != nil {
		return nil, err
	}
	var c Config
	err = cfg.MapTo(&c)
	return &c, err
}
