package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

type manager struct {
	paths paths
}

const ownershipMarker = ".skill-mgr"

func (m *manager) skills() ([]string, error) {
	entries, err := os.ReadDir(m.paths.library)
	if err != nil {
		return nil, fmt.Errorf("read skill library: %w", err)
	}
	var names []string
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join(m.paths.library, entry.Name(), "SKILL.md"))
		if err == nil && info.Mode().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

func (m *manager) selection(project string) (map[string]bool, error) {
	value, err := m.load()
	if err != nil {
		return nil, err
	}
	selected := make(map[string]bool)
	for _, name := range value.Projects[project] {
		selected[name] = true
	}
	return selected, nil
}

func (m *manager) toggle(project, skill string) (bool, error) {
	value, err := m.load()
	if err != nil {
		return false, err
	}
	selected := value.Projects[project]
	if index := slices.Index(selected, skill); index >= 0 {
		destination := m.installPath(project, skill)
		backup, err := os.MkdirTemp(filepath.Dir(destination), ".skill-mgr-remove-")
		if err != nil {
			return true, err
		}
		if err := os.Remove(backup); err != nil {
			return true, err
		}
		if err := captureManaged(destination, backup, skill); err != nil {
			return true, err
		}
		selected = slices.Delete(selected, index, index+1)
		if len(selected) == 0 {
			delete(value.Projects, project)
		} else {
			value.Projects[project] = selected
		}
		if err := m.save(value); err != nil {
			return true, errors.Join(err, os.Rename(backup, destination))
		}
		return false, os.RemoveAll(backup)
	}

	if _, err := os.Lstat(m.installPath(project, skill)); err == nil {
		return false, fmt.Errorf("%s already exists and is not managed", m.installPath(project, skill))
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := m.install(project, skill); err != nil {
		return false, err
	}
	value.Projects[project] = append(selected, skill)
	slices.Sort(value.Projects[project])
	if err := m.save(value); err != nil {
		os.RemoveAll(m.installPath(project, skill))
		return false, err
	}
	return true, nil
}

func (m *manager) syncAll() (int, error) {
	value, err := m.load()
	if err != nil {
		return 0, err
	}
	count := 0
	var syncErrors []error
	for project, skills := range value.Projects {
		for _, skill := range skills {
			if err := m.install(project, skill); err != nil {
				syncErrors = append(syncErrors, fmt.Errorf("%s/%s: %w", project, skill, err))
			} else {
				count++
			}
		}
	}
	return count, errors.Join(syncErrors...)
}

func (m *manager) install(project, skill string) error {
	source, err := filepath.EvalSymlinks(filepath.Join(m.paths.library, skill))
	if err != nil {
		return fmt.Errorf("resolve skill %s: %w", skill, err)
	}
	destination := m.installPath(project, skill)
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".skill-mgr-")
	if err != nil {
		return err
	}
	if err := os.Remove(stage); err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.CopyFS(stage, os.DirFS(source)); err != nil {
		return err
	}
	marker := filepath.Join(stage, ownershipMarker)
	if err := os.RemoveAll(marker); err != nil {
		return err
	}
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	backup := stage + ".old"
	_, writeErr := file.WriteString(skill)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := captureManaged(destination, backup, skill); errors.Is(err, os.ErrNotExist) {
		return os.Rename(stage, destination)
	} else if err != nil {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		return errors.Join(err, os.Rename(backup, destination))
	}
	return os.RemoveAll(backup)
}

func (m *manager) installPath(project, skill string) string {
	return filepath.Join(project, ".agents", "skills", skill)
}

func captureManaged(destination, backup, skill string) error {
	if err := os.Rename(destination, backup); err != nil {
		return err
	}
	if owns(backup, skill) {
		return nil
	}
	return errors.Join(
		fmt.Errorf("%s is no longer managed", destination),
		os.Rename(backup, destination),
	)
}

func owns(directory, skill string) bool {
	data, err := os.ReadFile(filepath.Join(directory, ownershipMarker))
	return err == nil && string(data) == skill
}
