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

type placeholderChange struct {
	base    string
	name    string
	enabled bool
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
	return m.changeRemotePlaceholdersAt(base, name, frontmatter, enabled)
}

// changeRemotePlaceholdersAt applies a placeholder change at an explicit
// harness root. Relocating authored content may need to update both the global
// root and the current project, independently of the TUI's selection layer.
func (m *manager) changeRemotePlaceholdersAt(
	base, name, frontmatter string,
	enabled bool,
) (func() error, error) {
	content, err := placeholderContent(name, frontmatter)
	if err != nil {
		return nil, err
	}
	marker := []byte(remotePlaceholderMarker)
	rootDirs := []string{
		filepath.Join(".agents", "skills"),
		filepath.Join(".claude", "skills"),
	}
	restoreContent := func(rootDir string, previous []byte) error {
		root, err := os.OpenRoot(base)
		if err != nil {
			return fmt.Errorf("open remote skill placeholder root %s: %w", base, err)
		}
		defer root.Close()
		skillDir := filepath.Join(rootDir, name)
		skillPath := filepath.Join(skillDir, "SKILL.md")
		markerPath := filepath.Join(skillDir, ".skills-mgr-placeholder")
		skillInfo, skillErr := root.Lstat(skillPath)
		markerInfo, markerErr := root.Lstat(markerPath)
		markerData, readErr := root.ReadFile(markerPath)
		if skillErr != nil || markerErr != nil || readErr != nil ||
			!skillInfo.Mode().IsRegular() || !markerInfo.Mode().IsRegular() ||
			!bytes.Equal(markerData, marker) {
			return fmt.Errorf("restore remote skill placeholder %s: managed files changed", skillPath)
		}
		if err := root.WriteFile(skillPath, previous, 0o644); err != nil {
			return fmt.Errorf("restore remote skill placeholder %s: %w", skillPath, err)
		}
		return nil
	}
	type removedPlaceholder struct {
		skillData  []byte
		markerData []byte
		skillMode  os.FileMode
		markerMode os.FileMode
		dirMode    os.FileMode
	}
	restoreRemoved := func(rootDir string, previous removedPlaceholder) error {
		root, err := os.OpenRoot(base)
		if err != nil {
			return fmt.Errorf("open remote skill placeholder root %s: %w", base, err)
		}
		defer root.Close()
		skillDir := filepath.Join(rootDir, name)
		skillPath := filepath.Join(skillDir, "SKILL.md")
		markerPath := filepath.Join(skillDir, ".skills-mgr-placeholder")
		if err := root.MkdirAll(rootDir, 0o755); err != nil {
			return fmt.Errorf("restore remote skill placeholder root %s: %w", rootDir, err)
		}
		if err := root.Mkdir(skillDir, previous.dirMode.Perm()); err != nil &&
			!errors.Is(err, os.ErrExist) {
			return fmt.Errorf("restore remote skill placeholder directory %s: %w", skillDir, err)
		}
		write := func(path string, data []byte, mode os.FileMode) error {
			file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
			if err != nil {
				return err
			}
			_, writeErr := file.Write(data)
			closeErr := file.Close()
			chmodErr := error(nil)
			if writeErr == nil && closeErr == nil {
				chmodErr = root.Chmod(path, mode.Perm())
			}
			return errors.Join(writeErr, closeErr, chmodErr)
		}
		if err := write(skillPath, previous.skillData, previous.skillMode); err != nil {
			return fmt.Errorf("restore remote skill placeholder %s: %w", skillPath, err)
		}
		if err := write(markerPath, previous.markerData, previous.markerMode); err != nil {
			cleanupErr := root.Remove(skillPath)
			return errors.Join(
				fmt.Errorf("restore remote skill placeholder marker %s: %w", markerPath, err),
				cleanupErr,
			)
		}
		if err := root.Chmod(skillDir, previous.dirMode.Perm()); err != nil {
			return fmt.Errorf("restore remote skill placeholder directory %s mode: %w", skillDir, err)
		}
		return nil
	}
	type mutation struct {
		rootDir  string
		previous []byte
		removed  *removedPlaceholder
	}

	apply := func(rootDir string, create bool) (mutation, bool, error) {
		root, err := os.OpenRoot(base)
		if err != nil {
			return mutation{}, false, fmt.Errorf("open remote skill placeholder root %s: %w", base, err)
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
				return mutation{}, false, fmt.Errorf("inspect remote skill placeholder path %s: %w", current, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if isRemotePlaceholderRootAlias(base, filepath.Dir(rootDir), current) {
					return mutation{}, false, nil
				}
				return mutation{}, false, fmt.Errorf("remote skill placeholder path %s contains a symbolic link", current)
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
			return mutation{}, false, fmt.Errorf("inspect remote skill placeholder %s: %w", skillPath, err)
		}
		markerData, markerExists, err := read(markerPath)
		if err != nil {
			return mutation{}, false, fmt.Errorf("inspect remote skill placeholder marker %s: %w", markerPath, err)
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
				return mutation{}, false, nil
			}
			if managed {
				skillInfo, skillErr := root.Lstat(skillPath)
				markerInfo, markerErr := root.Lstat(markerPath)
				if skillErr != nil || markerErr != nil || !skillInfo.Mode().IsRegular() || !markerInfo.Mode().IsRegular() {
					return mutation{}, false, fmt.Errorf("update remote skill placeholder %s: managed files are not regular", skillPath)
				}
				if err := root.WriteFile(skillPath, content, 0o644); err != nil {
					restoreErr := root.WriteFile(skillPath, skillData, 0o644)
					return mutation{}, false, errors.Join(
						fmt.Errorf("update remote skill placeholder %s: %w", skillPath, err),
						restoreErr,
					)
				}
				return mutation{rootDir: rootDir, previous: slices.Clone(skillData)}, true, nil
			}
			if !vacant {
				return mutation{}, false, fmt.Errorf("create remote skill placeholder %s: path already exists", skillPath)
			}
			if err := root.MkdirAll(rootDir, 0o755); err != nil {
				return mutation{}, false, fmt.Errorf("create remote skill placeholder root %s: %w", rootDir, err)
			}
			skillDirCreated := false
			if err := root.Mkdir(skillDir, 0o755); err == nil {
				skillDirCreated = true
			} else if !errors.Is(err, os.ErrExist) {
				return mutation{}, false, fmt.Errorf("create remote skill placeholder directory %s: %w", skillDir, err)
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
				return mutation{}, false, errors.Join(
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
				return mutation{}, false, errors.Join(
					fmt.Errorf("write remote skill placeholder marker %s: %w", markerPath, err),
					cleanupErr,
				)
			}
			return mutation{rootDir: rootDir}, true, nil
		}

		if !managed {
			return mutation{}, false, nil
		}
		skillInfo, skillErr := root.Lstat(skillPath)
		markerInfo, markerErr := root.Lstat(markerPath)
		dirInfo, dirErr := root.Lstat(skillDir)
		if skillErr != nil || markerErr != nil || dirErr != nil ||
			!skillInfo.Mode().IsRegular() || !markerInfo.Mode().IsRegular() ||
			!dirInfo.IsDir() {
			return mutation{}, false, fmt.Errorf("remove remote skill placeholder %s: managed paths changed", skillPath)
		}
		previous := &removedPlaceholder{
			skillData:  slices.Clone(skillData),
			markerData: slices.Clone(markerData),
			skillMode:  skillInfo.Mode(),
			markerMode: markerInfo.Mode(),
			dirMode:    dirInfo.Mode(),
		}
		if err := root.Remove(markerPath); err != nil {
			return mutation{}, false, fmt.Errorf("remove remote skill placeholder marker %s: %w", markerPath, err)
		}
		if err := root.Remove(skillPath); err != nil {
			markerCreated, restoreErr := write(markerPath, marker)
			if restoreErr != nil && markerCreated {
				restoreErr = errors.Join(restoreErr, remove(markerPath))
			}
			return mutation{}, false, errors.Join(
				fmt.Errorf("remove remote skill placeholder %s: %w", skillPath, err),
				restoreErr,
			)
		}
		_ = root.Remove(skillDir)
		return mutation{rootDir: rootDir, removed: previous}, true, nil
	}

	var mutations []mutation
	rollback := func() error {
		rollbackErr := error(nil)
		for _, mutation := range slices.Backward(mutations) {
			var undoErr error
			if mutation.removed != nil {
				undoErr = restoreRemoved(mutation.rootDir, *mutation.removed)
			} else if mutation.previous != nil {
				undoErr = restoreContent(mutation.rootDir, mutation.previous)
			} else {
				_, _, undoErr = apply(mutation.rootDir, !enabled)
			}
			rollbackErr = errors.Join(rollbackErr, undoErr)
		}
		return rollbackErr
	}
	for _, rootDir := range rootDirs {
		mutation, didChange, err := apply(rootDir, enabled)
		if err == nil {
			if didChange {
				mutations = append(mutations, mutation)
			}
			continue
		}
		return nil, errors.Join(err, rollback())
	}
	if len(mutations) == 0 {
		return nil, nil
	}
	return rollback, nil
}

func (m *manager) changeRemotePlaceholdersAcross(
	changes []placeholderChange,
	frontmatter string,
) (func() error, error) {
	var undos []func() error
	rollback := func() error {
		rollbackErr := error(nil)
		for _, undo := range slices.Backward(undos) {
			rollbackErr = errors.Join(rollbackErr, undo())
		}
		return rollbackErr
	}
	for _, change := range changes {
		undo, err := m.changeRemotePlaceholdersAt(
			change.base,
			change.name,
			frontmatter,
			change.enabled,
		)
		if err != nil {
			return nil, errors.Join(err, rollback())
		}
		if undo != nil {
			undos = append(undos, undo)
		}
	}
	if len(undos) == 0 {
		return nil, nil
	}
	return rollback, nil
}
