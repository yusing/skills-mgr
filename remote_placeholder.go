package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
)

// The marker text is load-bearing on disk: ownership checks compare these exact
// bytes, so changing it would orphan every placeholder already written.
const remotePlaceholderMarker = "skills-mgr remote placeholder\n"

type placeholderFrontmatter struct {
	Name                   string `yaml:"name"`
	Description            string `yaml:"description"`
	DisableModelInvocation bool   `yaml:"disable-model-invocation"`
}

// placeholderContent renders the stub a harness loads in place of a managed
// skill. It carries frontmatter and no body, and forces
// disable-model-invocation so the harness offers the name in its slash-command
// menu without advertising the skill to the model. That leaves skills-mgr list
// as the only model-facing view of the enabled set, which is what lets a
// placeholder outlive a false condition harmlessly.
func placeholderContent(name, frontmatter string) ([]byte, error) {
	var metadata skillFrontmatter
	if err := yaml.Unmarshal([]byte(frontmatter), &metadata); err != nil {
		return nil, fmt.Errorf("decode frontmatter for placeholder %s: %w", name, err)
	}
	rendered, err := yaml.Marshal(placeholderFrontmatter{
		Name:                   name,
		Description:            metadata.Description,
		DisableModelInvocation: true,
	})
	if err != nil {
		return nil, fmt.Errorf("render placeholder frontmatter for %s: %w", name, err)
	}
	return fmt.Appendf(nil, "---\n%s---\n", rendered), nil
}

func isRemotePlaceholderRootAlias(base, source, path string) bool {
	if path != source && !strings.HasPrefix(path, source+string(filepath.Separator)) {
		return false
	}

	suffix := strings.TrimPrefix(path, source)
	linkedInfo, err := os.Stat(filepath.Join(base, path))
	if err != nil || !linkedInfo.IsDir() {
		return false
	}
	for _, candidate := range [...]string{".agents", ".claude", ".codex", ".grok"} {
		if candidate == source {
			continue
		}
		candidateInfo, err := os.Stat(filepath.Join(base, candidate, suffix))
		if err == nil && candidateInfo.IsDir() && os.SameFile(linkedInfo, candidateInfo) {
			return true
		}
	}
	return false
}

func (m *manager) setRemotePlaceholders(
	project, name, frontmatter string,
	enabled bool,
) error {
	_, err := m.changeRemotePlaceholders(project, name, frontmatter, enabled)
	return err
}

func (m *manager) changeRemotePlaceholders(
	project, name, frontmatter string,
	enabled bool,
) (func() error, error) {
	base := project
	if m.global {
		base = m.paths.placeholderDir
	}
	content, err := placeholderContent(name, frontmatter)
	if err != nil {
		return nil, err
	}
	marker := []byte(remotePlaceholderMarker)
	rootDirs := []string{
		filepath.Join(".agents", "skills"),
		filepath.Join(".claude", "skills"),
	}

	apply := func(rootDir string, create bool) (bool, error) {
		root, err := os.OpenRoot(base)
		if err != nil {
			return false, fmt.Errorf("open remote skill placeholder root %s: %w", base, err)
		}
		defer root.Close()

		skillDir := filepath.Join(rootDir, name)
		skillPath := filepath.Join(skillDir, "SKILL.md")
		markerPath := filepath.Join(skillDir, ".skills-mgr-placeholder")
		current := ""
		for component := range strings.SplitSeq(filepath.ToSlash(skillDir), "/") {
			current = filepath.Join(current, filepath.FromSlash(component))
			info, err := root.Lstat(current)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return false, fmt.Errorf("inspect remote skill placeholder path %s: %w", current, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if isRemotePlaceholderRootAlias(base, filepath.Dir(rootDir), current) {
					return false, nil
				}
				return false, fmt.Errorf("remote skill placeholder path %s contains a symbolic link", current)
			}
		}

		read := func(path string) ([]byte, bool, error) {
			data, err := root.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				return nil, false, nil
			}
			return data, err == nil, err
		}
		skillData, skillExists, err := read(skillPath)
		if err != nil {
			return false, fmt.Errorf("inspect remote skill placeholder %s: %w", skillPath, err)
		}
		markerData, markerExists, err := read(markerPath)
		if err != nil {
			return false, fmt.Errorf("inspect remote skill placeholder marker %s: %w", markerPath, err)
		}
		managed := skillExists && markerExists && bytes.Equal(markerData, marker)
		owned := managed && bytes.Equal(skillData, content)
		vacant := !skillExists && !markerExists

		remove := func(path string) error {
			err := root.Remove(path)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		write := func(path string, data []byte) (bool, error) {
			file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				return false, err
			}
			_, writeErr := file.Write(data)
			return true, errors.Join(writeErr, file.Close())
		}

		if create {
			if owned {
				return false, nil
			}
			if managed {
				skillInfo, skillErr := root.Lstat(skillPath)
				markerInfo, markerErr := root.Lstat(markerPath)
				if skillErr != nil || markerErr != nil || !skillInfo.Mode().IsRegular() || !markerInfo.Mode().IsRegular() {
					return false, fmt.Errorf("update remote skill placeholder %s: managed files are not regular", skillPath)
				}
				if err := root.WriteFile(skillPath, content, 0o644); err != nil {
					restoreErr := root.WriteFile(skillPath, skillData, 0o644)
					return false, errors.Join(
						fmt.Errorf("update remote skill placeholder %s: %w", skillPath, err),
						restoreErr,
					)
				}
				return false, nil
			}
			if !vacant {
				return false, fmt.Errorf("create remote skill placeholder %s: path already exists", skillPath)
			}
			if err := root.MkdirAll(rootDir, 0o755); err != nil {
				return false, fmt.Errorf("create remote skill placeholder root %s: %w", rootDir, err)
			}
			skillDirCreated := false
			if err := root.Mkdir(skillDir, 0o755); err == nil {
				skillDirCreated = true
			} else if !errors.Is(err, os.ErrExist) {
				return false, fmt.Errorf("create remote skill placeholder directory %s: %w", skillDir, err)
			}
			cleanupSkillDir := func() error {
				if !skillDirCreated {
					return nil
				}
				return remove(skillDir)
			}
			skillCreated, err := write(skillPath, content)
			if err != nil {
				cleanupErr := cleanupSkillDir()
				if skillCreated {
					cleanupErr = errors.Join(remove(skillPath), cleanupErr)
				}
				return false, errors.Join(
					fmt.Errorf("write remote skill placeholder %s: %w", skillPath, err),
					cleanupErr,
				)
			}
			markerCreated, err := write(markerPath, marker)
			if err != nil {
				cleanupErr := error(nil)
				if markerCreated {
					cleanupErr = remove(markerPath)
				}
				if skillCreated {
					cleanupErr = errors.Join(cleanupErr, remove(skillPath))
				}
				cleanupErr = errors.Join(cleanupErr, cleanupSkillDir())
				return false, errors.Join(
					fmt.Errorf("write remote skill placeholder marker %s: %w", markerPath, err),
					cleanupErr,
				)
			}
			return true, nil
		}

		if !managed {
			return false, nil
		}
		if err := root.Remove(markerPath); err != nil {
			return false, fmt.Errorf("remove remote skill placeholder marker %s: %w", markerPath, err)
		}
		if err := root.Remove(skillPath); err != nil {
			markerCreated, restoreErr := write(markerPath, marker)
			if restoreErr != nil && markerCreated {
				restoreErr = errors.Join(restoreErr, remove(markerPath))
			}
			return false, errors.Join(
				fmt.Errorf("remove remote skill placeholder %s: %w", skillPath, err),
				restoreErr,
			)
		}
		_ = root.Remove(skillDir)
		return true, nil
	}

	var changed []string
	rollback := func() error {
		rollbackErr := error(nil)
		for _, rootDir := range slices.Backward(changed) {
			_, undoErr := apply(rootDir, !enabled)
			rollbackErr = errors.Join(rollbackErr, undoErr)
		}
		return rollbackErr
	}
	for _, rootDir := range rootDirs {
		didChange, err := apply(rootDir, enabled)
		if err == nil {
			if didChange {
				changed = append(changed, rootDir)
			}
			continue
		}
		return nil, errors.Join(err, rollback())
	}
	if len(changed) == 0 {
		return nil, nil
	}
	return rollback, nil
}
