package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type relativeLink struct {
	path   string
	before string
	after  string
}

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
	links, err := findRelativeLinks(m.paths.source, m.paths.library)
	if err != nil {
		return 0, fmt.Errorf("read skill links: %w", err)
	}
	for index, link := range links {
		if err := replaceSymlink(link.path, link.after); err != nil {
			return 0, errors.Join(
				fmt.Errorf("rebase %s: %w", link.path, err),
				restoreLinks(links[:index]),
			)
		}
	}
	if err := os.Rename(m.paths.source, m.paths.library); err != nil {
		return 0, errors.Join(
			fmt.Errorf("move skills into manager: %w", err),
			restoreLinks(links),
		)
	}
	skills, err := m.skills()
	return len(skills), err
}

func findRelativeLinks(source, library string) ([]relativeLink, error) {
	var links []relativeLink
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.Type()&os.ModeSymlink == 0 {
			return err
		}
		target, err := os.Readlink(path)
		if err != nil || filepath.IsAbs(target) {
			return err
		}
		oldTarget := resolveKnownTarget(path, target)
		if withinSource, err := filepath.Rel(source, oldTarget); err == nil && filepath.IsLocal(withinSource) {
			oldTarget = filepath.Join(library, withinSource)
		}
		linkPath, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		newTarget, err := filepath.Rel(filepath.Dir(filepath.Join(library, linkPath)), oldTarget)
		if err != nil {
			return err
		}
		if newTarget == target {
			return nil
		}
		links = append(links, relativeLink{path: path, before: target, after: newTarget})
		return nil
	})
	return links, err
}

func resolveKnownTarget(linkPath, target string) string {
	parts := strings.Split(filepath.ToSlash(target), "/")
	base := filepath.Dir(linkPath)
	for index := len(parts); index >= 0; index-- {
		prefix := filepath.FromSlash(strings.Join(parts[:index], "/"))
		resolved, err := filepath.EvalSymlinks(base + string(filepath.Separator) + prefix)
		if err != nil {
			continue
		}
		suffix := filepath.FromSlash(strings.Join(parts[index:], "/"))
		return filepath.Join(resolved, suffix)
	}
	return filepath.Clean(filepath.Join(base, target))
}

func restoreLinks(links []relativeLink) error {
	var result error
	for _, link := range links {
		result = errors.Join(result, replaceSymlink(link.path, link.before))
	}
	return result
}

func replaceSymlink(path, target string) error {
	temp, err := os.MkdirTemp(filepath.Dir(path), ".skill-mgr-link-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	link := filepath.Join(temp, "link")
	if err := os.Symlink(target, link); err != nil {
		return err
	}
	return os.Rename(link, path)
}
