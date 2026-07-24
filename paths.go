package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type paths struct {
	userSkills     string
	codexHome      string
	adminSkills    string
	remoteRegistry string
	skillsMP       string
	remoteSkills   string
}

func defaultPaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, fmt.Errorf("find home directory: %w", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return paths{}, fmt.Errorf("find cache directory: %w", err)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	return paths{
		userSkills:     filepath.Join(home, ".agents", "skills"),
		codexHome:      codexHome,
		adminSkills:    "/etc/codex/skills",
		remoteRegistry: filepath.Join(cache, "skills-mgr", "skills-sh.json"),
		skillsMP:       filepath.Join(cache, "skills-mgr", "skillsmp.json"),
		remoteSkills:   filepath.Join(cache, "skills-mgr", "remote-skills"),
	}, nil
}
