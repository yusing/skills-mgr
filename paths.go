package main

import (
	"fmt"
	"os"
	"path/filepath"
)

type paths struct {
	library string
	state   string
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
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(home, ".local", "state")
	}
	return paths{
		library: filepath.Join(data, "skill-mgr", "skills"),
		state:   filepath.Join(state, "skill-mgr", "state.json"),
		source:  filepath.Join(home, ".agents", "skills"),
	}, nil
}
