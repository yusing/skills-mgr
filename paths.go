package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type paths struct {
	library string
	source  string
}

func defaultPaths() (paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, fmt.Errorf("find home directory: %w", err)
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return paths{
		library: filepath.Join(data, "skill-mgr", "skills"),
		source:  filepath.Join(home, ".agents", "skills"),
	}, nil
}
