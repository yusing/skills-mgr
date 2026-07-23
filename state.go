package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type state struct {
	Projects map[string][]string `json:"projects"`
}

func (m *manager) load() (state, error) {
	data, err := os.ReadFile(m.paths.state)
	if errors.Is(err, os.ErrNotExist) {
		return state{Projects: make(map[string][]string)}, nil
	}
	if err != nil {
		return state{}, fmt.Errorf("read state: %w", err)
	}
	var result state
	if err := json.Unmarshal(data, &result); err != nil {
		return state{}, fmt.Errorf("decode state: %w", err)
	}
	if result.Projects == nil {
		result.Projects = make(map[string][]string)
	}
	return result, nil
}

func (m *manager) save(value state) error {
	dir := filepath.Dir(m.paths.state)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".state-")
	if err != nil {
		return fmt.Errorf("create state: %w", err)
	}
	name := temp.Name()
	defer os.Remove(name)
	err = json.NewEncoder(temp).Encode(value)
	err = errors.Join(err, temp.Close())
	if err == nil {
		err = os.Rename(name, m.paths.state)
	}
	return err
}
