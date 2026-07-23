package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type paths struct {
	userSkills  string
	codexHome   string
	adminSkills string
}

func defaultPaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, fmt.Errorf("find home directory: %w", err)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	return paths{
		userSkills:  filepath.Join(home, ".agents", "skills"),
		codexHome:   codexHome,
		adminSkills: "/etc/codex/skills",
	}, nil
}
