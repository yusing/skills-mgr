package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func (m *manager) migrate() (int, error) {
	_, sourceErr := os.Stat(m.paths.source)
	_, libraryErr := os.Stat(m.paths.library)

	if errors.Is(sourceErr, os.ErrNotExist) && libraryErr == nil {
		skills, err := m.skills()
		return len(skills), err
	}
	if sourceErr != nil {
		return 0, fmt.Errorf("read %s: %w", m.paths.source, sourceErr)
	}
	if libraryErr == nil {
		return 0, fmt.Errorf("%s already exists", m.paths.library)
	}
	if !errors.Is(libraryErr, os.ErrNotExist) {
		return 0, fmt.Errorf("read %s: %w", m.paths.library, libraryErr)
	}
	if err := os.MkdirAll(filepath.Dir(m.paths.library), 0o755); err != nil {
		return 0, fmt.Errorf("create manager directory: %w", err)
	}
	if err := os.Rename(m.paths.source, m.paths.library); err != nil {
		return 0, fmt.Errorf("move skills into manager: %w", err)
	}
	skills, err := m.skills()
	return len(skills), err
}
