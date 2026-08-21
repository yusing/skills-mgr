package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type paths struct {
	userSkills     string
	claudeSkills   string
	grokSkills     string
	codexHome      string
	adminSkills    string
	globalLockDir  string
	selectionLocks string
	remoteRegistry string
	skillsMP       string
	remoteSkills   string
	daemonSocket   string
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
	socketDir := os.Getenv("XDG_RUNTIME_DIR")
	if socketDir == "" {
		socketDir = filepath.Join(cache, "skills-mgr")
	}
	return paths{
		userSkills:     filepath.Join(home, ".agents", "skills"),
		claudeSkills:   filepath.Join(home, ".claude", "skills"),
		grokSkills:     filepath.Join(home, ".grok", "skills"),
		codexHome:      codexHome,
		adminSkills:    "/etc/codex/skills",
		globalLockDir:  home,
		selectionLocks: filepath.Join(cache, "skills-mgr", "selection-locks"),
		remoteRegistry: filepath.Join(cache, "skills-mgr", "skills-sh.json"),
		skillsMP:       filepath.Join(cache, "skills-mgr", "skillsmp.json"),
		remoteSkills:   filepath.Join(cache, "skills-mgr", "remote-skills"),
		daemonSocket:   filepath.Join(socketDir, "skills-mgr.sock"),
	}, nil
}
