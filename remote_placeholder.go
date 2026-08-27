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

// samePlaceholderRoot prevents aliased roots from producing overlapping plans
// whose mutations would invalidate each other during revalidation.
func samePlaceholderRoot(first, second string) (bool, error) {
	if filepath.Clean(first) == filepath.Clean(second) {
		return true, nil
	}
	firstInfo, err := os.Stat(first)
	if err != nil {
		return false, fmt.Errorf("inspect remote skill placeholder root %s: %w", first, err)
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		return false, fmt.Errorf("inspect remote skill placeholder root %s: %w", second, err)
	}
	return os.SameFile(firstInfo, secondInfo), nil
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

type placeholderMutationKind uint8

const (
	placeholderCreate placeholderMutationKind = iota
	placeholderUpdate
	placeholderRemove
)

type removedPlaceholder struct {
	skillData  []byte
	skillMode  os.FileMode
	markerMode os.FileMode
	dirMode    os.FileMode
}

type placeholderMutation struct {
	base        string
	rootDir     string
	name        string
	kind        placeholderMutationKind
	content     []byte
	marker      []byte
	previous    []byte
	removed     *removedPlaceholder
	missingDirs []string
	createdDirs []string
}

type placeholderState struct {
	skip         bool
	skillData    []byte
	markerData   []byte
	skillMode    os.FileMode
	markerMode   os.FileMode
	dirMode      os.FileMode
	skillExists  bool
	markerExists bool
	dirExists    bool
	missingDirs  []string
}

func inspectRemotePlaceholder(
	base, rootDir, name string,
) (placeholderState, error) {
	root, err := os.OpenRoot(base)
	if err != nil {
		return placeholderState{}, fmt.Errorf("open remote skill placeholder root %s: %w", base, err)
	}
	defer root.Close()

	skillDir := filepath.Join(rootDir, name)
	skillPath := filepath.Join(skillDir, "SKILL.md")
	markerPath := filepath.Join(skillDir, ".skills-mgr-placeholder")
	state := placeholderState{}
	current := ""
	for component := range strings.SplitSeq(filepath.ToSlash(skillDir), "/") {
		current = filepath.Join(current, filepath.FromSlash(component))
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			state.missingDirs = append(state.missingDirs, current)
			continue
		}
		if err != nil {
			return placeholderState{}, fmt.Errorf(
				"inspect remote skill placeholder path %s: %w",
				current,
				err,
			)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if isRemotePlaceholderRootAlias(base, filepath.Dir(rootDir), current) {
				return placeholderState{skip: true}, nil
			}
			return placeholderState{}, fmt.Errorf(
				"remote skill placeholder path %s contains a symbolic link",
				current,
			)
		}
	}

	read := func(path string) ([]byte, bool, error) {
		data, err := root.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return data, err == nil, err
	}
	state.skillData, state.skillExists, err = read(skillPath)
	if err != nil {
		return placeholderState{}, fmt.Errorf(
			"inspect remote skill placeholder %s: %w",
			skillPath,
			err,
		)
	}
	state.markerData, state.markerExists, err = read(markerPath)
	if err != nil {
		return placeholderState{}, fmt.Errorf(
			"inspect remote skill placeholder marker %s: %w",
			markerPath,
			err,
		)
	}
	if state.skillExists {
		info, err := root.Lstat(skillPath)
		if err != nil {
			return placeholderState{}, fmt.Errorf(
				"inspect remote skill placeholder %s: %w",
				skillPath,
				err,
			)
		}
		state.skillMode = info.Mode()
	}
	if state.markerExists {
		info, err := root.Lstat(markerPath)
		if err != nil {
			return placeholderState{}, fmt.Errorf(
				"inspect remote skill placeholder marker %s: %w",
				markerPath,
				err,
			)
		}
		state.markerMode = info.Mode()
	}
	info, err := root.Lstat(skillDir)
	switch {
	case err == nil:
		state.dirExists = true
		state.dirMode = info.Mode()
	case errors.Is(err, os.ErrNotExist):
	default:
		return placeholderState{}, fmt.Errorf(
			"inspect remote skill placeholder directory %s: %w",
			skillDir,
			err,
		)
	}
	return state, nil
}

func planRemotePlaceholder(
	base, rootDir, name string,
	content, marker []byte,
	create bool,
) (placeholderMutation, bool, error) {
	state, err := inspectRemotePlaceholder(base, rootDir, name)
	if err != nil {
		return placeholderMutation{}, false, err
	}
	if state.skip {
		return placeholderMutation{}, false, nil
	}
	managed := state.skillExists &&
		state.markerExists &&
		bytes.Equal(state.markerData, marker)
	planned := placeholderMutation{
		base:    base,
		rootDir: rootDir,
		name:    name,
		content: slices.Clone(content),
		marker:  slices.Clone(marker),
	}

	if create {
		owned := managed && bytes.Equal(state.skillData, content)
		vacant := !state.skillExists && !state.markerExists
		switch {
		case owned:
			return placeholderMutation{}, false, nil
		case managed:
			if !state.skillMode.IsRegular() || !state.markerMode.IsRegular() {
				return placeholderMutation{}, false, fmt.Errorf(
					"update remote skill placeholder %s: managed files are not regular",
					filepath.Join(rootDir, name, "SKILL.md"),
				)
			}
			planned.kind = placeholderUpdate
			planned.previous = slices.Clone(state.skillData)
			return planned, true, nil
		case !vacant:
			return placeholderMutation{}, false, fmt.Errorf(
				"create remote skill placeholder %s: path already exists",
				filepath.Join(rootDir, name, "SKILL.md"),
			)
		default:
			planned.kind = placeholderCreate
			planned.missingDirs = slices.Clone(state.missingDirs)
			return planned, true, nil
		}
	}

	if !managed {
		return placeholderMutation{}, false, nil
	}
	if !state.skillMode.IsRegular() ||
		!state.markerMode.IsRegular() ||
		!state.dirExists ||
		!state.dirMode.IsDir() {
		return placeholderMutation{}, false, fmt.Errorf(
			"remove remote skill placeholder %s: managed paths changed",
			filepath.Join(rootDir, name, "SKILL.md"),
		)
	}
	planned.kind = placeholderRemove
	planned.removed = &removedPlaceholder{
		skillData:  slices.Clone(state.skillData),
		skillMode:  state.skillMode,
		markerMode: state.markerMode,
		dirMode:    state.dirMode,
	}
	return planned, true, nil
}

func planRemotePlaceholdersAt(
	base, name, frontmatter string,
	enabled bool,
) ([]placeholderMutation, error) {
	content, err := placeholderContent(name, frontmatter)
	if err != nil {
		return nil, err
	}
	marker := []byte(remotePlaceholderMarker)
	rootDirs := [...]string{
		filepath.Join(".agents", "skills"),
		filepath.Join(".claude", "skills"),
	}
	var planned []placeholderMutation
	for _, rootDir := range rootDirs {
		mutation, changed, err := planRemotePlaceholder(
			base,
			rootDir,
			name,
			content,
			marker,
			enabled,
		)
		if err != nil {
			return nil, err
		}
		if changed {
			planned = append(planned, mutation)
		}
	}
	return planned, nil
}

func writePlaceholderFile(
	root *os.Root,
	path string,
	data []byte,
	mode os.FileMode,
) (bool, error) {
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return false, err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	chmodErr := error(nil)
	if writeErr == nil && closeErr == nil {
		chmodErr = root.Chmod(path, mode.Perm())
	}
	return true, errors.Join(writeErr, closeErr, chmodErr)
}

func removePlaceholderPath(root *os.Root, path string) error {
	err := root.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func restorePlaceholderFile(
	root *os.Root,
	path string,
	data []byte,
	mode os.FileMode,
) error {
	created, err := writePlaceholderFile(root, path, data, mode)
	if err == nil {
		return nil
	}
	if created {
		return errors.Join(err, removePlaceholderPath(root, path))
	}
	return err
}

func removePlaceholderDirs(root *os.Root, paths []string) error {
	var result error
	for index := len(paths) - 1; index >= 0; index-- {
		result = errors.Join(result, removePlaceholderPath(root, paths[index]))
	}
	return result
}

func (mutation placeholderMutation) apply() (func() error, error) {
	state, err := inspectRemotePlaceholder(mutation.base, mutation.rootDir, mutation.name)
	if err != nil {
		return nil, err
	}
	skillDir := filepath.Join(mutation.rootDir, mutation.name)
	skillPath := filepath.Join(skillDir, "SKILL.md")
	markerPath := filepath.Join(skillDir, ".skills-mgr-placeholder")
	managed := state.skillExists &&
		state.markerExists &&
		bytes.Equal(state.markerData, mutation.marker)

	switch mutation.kind {
	case placeholderCreate:
		if state.skip ||
			state.skillExists ||
			state.markerExists ||
			!slices.Equal(state.missingDirs, mutation.missingDirs) {
			return nil, fmt.Errorf(
				"create remote skill placeholder %s: path changed after planning",
				skillPath,
			)
		}
	case placeholderUpdate:
		if state.skip ||
			!managed ||
			!state.skillMode.IsRegular() ||
			!state.markerMode.IsRegular() ||
			!bytes.Equal(state.skillData, mutation.previous) {
			return nil, fmt.Errorf(
				"update remote skill placeholder %s: managed files changed",
				skillPath,
			)
		}
	case placeholderRemove:
		previous := mutation.removed
		if state.skip ||
			previous == nil ||
			!managed ||
			!state.dirExists ||
			state.skillMode != previous.skillMode ||
			state.markerMode != previous.markerMode ||
			state.dirMode != previous.dirMode ||
			!bytes.Equal(state.skillData, previous.skillData) {
			return nil, fmt.Errorf(
				"remove remote skill placeholder %s: managed paths changed",
				skillPath,
			)
		}
	default:
		return nil, fmt.Errorf("remote skill placeholder mutation is invalid")
	}

	root, err := os.OpenRoot(mutation.base)
	if err != nil {
		return nil, fmt.Errorf(
			"open remote skill placeholder root %s: %w",
			mutation.base,
			err,
		)
	}
	defer root.Close()

	switch mutation.kind {
	case placeholderCreate:
		createdDirs := make([]string, 0, len(mutation.missingDirs))
		for _, path := range mutation.missingDirs {
			if err := root.Mkdir(path, 0o755); err != nil {
				return nil, errors.Join(
					fmt.Errorf("create remote skill placeholder directory %s: %w", path, err),
					removePlaceholderDirs(root, createdDirs),
				)
			}
			createdDirs = append(createdDirs, path)
		}
		skillCreated, err := writePlaceholderFile(root, skillPath, mutation.content, 0o644)
		if err != nil {
			cleanupErr := error(nil)
			if skillCreated {
				cleanupErr = removePlaceholderPath(root, skillPath)
			}
			cleanupErr = errors.Join(cleanupErr, removePlaceholderDirs(root, createdDirs))
			return nil, errors.Join(
				fmt.Errorf("write remote skill placeholder %s: %w", skillPath, err),
				cleanupErr,
			)
		}
		markerCreated, err := writePlaceholderFile(root, markerPath, mutation.marker, 0o644)
		if err != nil {
			cleanupErr := error(nil)
			if markerCreated {
				cleanupErr = removePlaceholderPath(root, markerPath)
			}
			cleanupErr = errors.Join(
				cleanupErr,
				removePlaceholderPath(root, skillPath),
				removePlaceholderDirs(root, createdDirs),
			)
			return nil, errors.Join(
				fmt.Errorf("write remote skill placeholder marker %s: %w", markerPath, err),
				cleanupErr,
			)
		}
		mutation.createdDirs = createdDirs
	case placeholderUpdate:
		if err := root.WriteFile(skillPath, mutation.content, 0o644); err != nil {
			restoreErr := root.WriteFile(skillPath, mutation.previous, 0o644)
			return nil, errors.Join(
				fmt.Errorf("update remote skill placeholder %s: %w", skillPath, err),
				restoreErr,
			)
		}
	case placeholderRemove:
		previous := mutation.removed
		if err := root.Remove(markerPath); err != nil {
			return nil, fmt.Errorf(
				"remove remote skill placeholder marker %s: %w",
				markerPath,
				err,
			)
		}
		if err := root.Remove(skillPath); err != nil {
			restoreErr := restorePlaceholderFile(
				root,
				markerPath,
				mutation.marker,
				previous.markerMode,
			)
			return nil, errors.Join(
				fmt.Errorf("remove remote skill placeholder %s: %w", skillPath, err),
				restoreErr,
			)
		}
		_ = root.Remove(skillDir)
	}
	return mutation.undo, nil
}

func (mutation placeholderMutation) undo() error {
	state, err := inspectRemotePlaceholder(mutation.base, mutation.rootDir, mutation.name)
	if err != nil {
		return err
	}
	skillDir := filepath.Join(mutation.rootDir, mutation.name)
	skillPath := filepath.Join(skillDir, "SKILL.md")
	markerPath := filepath.Join(skillDir, ".skills-mgr-placeholder")
	managed := state.skillExists &&
		state.markerExists &&
		bytes.Equal(state.markerData, mutation.marker)
	root, err := os.OpenRoot(mutation.base)
	if err != nil {
		return fmt.Errorf(
			"open remote skill placeholder root %s: %w",
			mutation.base,
			err,
		)
	}
	defer root.Close()

	switch mutation.kind {
	case placeholderCreate:
		if state.skip ||
			!managed ||
			!bytes.Equal(state.skillData, mutation.content) {
			return fmt.Errorf(
				"restore remote skill placeholder %s: managed files changed",
				skillPath,
			)
		}
		if err := root.Remove(markerPath); err != nil {
			return fmt.Errorf(
				"restore remote skill placeholder marker %s: %w",
				markerPath,
				err,
			)
		}
		if err := root.Remove(skillPath); err != nil {
			restoreErr := restorePlaceholderFile(root, markerPath, mutation.marker, 0o644)
			return errors.Join(
				fmt.Errorf("restore remote skill placeholder %s: %w", skillPath, err),
				restoreErr,
			)
		}
		return removePlaceholderDirs(root, mutation.createdDirs)
	case placeholderUpdate:
		if state.skip ||
			!managed ||
			!bytes.Equal(state.skillData, mutation.content) {
			return fmt.Errorf(
				"restore remote skill placeholder %s: managed files changed",
				skillPath,
			)
		}
		if err := root.WriteFile(skillPath, mutation.previous, 0o644); err != nil {
			return fmt.Errorf("restore remote skill placeholder %s: %w", skillPath, err)
		}
		return nil
	case placeholderRemove:
		if state.skip || state.skillExists || state.markerExists {
			return fmt.Errorf(
				"restore remote skill placeholder %s: path changed",
				skillPath,
			)
		}
		previous := mutation.removed
		if previous == nil {
			return fmt.Errorf("restore remote skill placeholder %s: state is missing", skillPath)
		}
		if err := root.MkdirAll(mutation.rootDir, 0o755); err != nil {
			return fmt.Errorf(
				"restore remote skill placeholder root %s: %w",
				mutation.rootDir,
				err,
			)
		}
		if err := root.Mkdir(skillDir, previous.dirMode.Perm()); err != nil &&
			!errors.Is(err, os.ErrExist) {
			return fmt.Errorf(
				"restore remote skill placeholder directory %s: %w",
				skillDir,
				err,
			)
		}
		if err := restorePlaceholderFile(
			root,
			skillPath,
			previous.skillData,
			previous.skillMode,
		); err != nil {
			return fmt.Errorf("restore remote skill placeholder %s: %w", skillPath, err)
		}
		if err := restorePlaceholderFile(
			root,
			markerPath,
			mutation.marker,
			previous.markerMode,
		); err != nil {
			cleanupErr := removePlaceholderPath(root, skillPath)
			return errors.Join(
				fmt.Errorf(
					"restore remote skill placeholder marker %s: %w",
					markerPath,
					err,
				),
				cleanupErr,
			)
		}
		if err := root.Chmod(skillDir, previous.dirMode.Perm()); err != nil {
			return fmt.Errorf(
				"restore remote skill placeholder directory %s mode: %w",
				skillDir,
				err,
			)
		}
		return nil
	default:
		return fmt.Errorf("remote skill placeholder mutation is invalid")
	}
}

func applyRemotePlaceholderPlan(
	planned []placeholderMutation,
	journal *mutationJournal,
) error {
	for _, mutation := range planned {
		undo, err := mutation.apply()
		if err != nil {
			return err
		}
		journal.add(undo)
	}
	return nil
}

// changeRemotePlaceholdersAt applies a placeholder change at an explicit
// harness root. Relocating authored content may need to update both the global
// root and the current project, independently of the TUI's selection layer.
func (m *manager) changeRemotePlaceholdersAt(
	base, name, frontmatter string,
	enabled bool,
) (func() error, error) {
	planned, err := planRemotePlaceholdersAt(base, name, frontmatter, enabled)
	if err != nil {
		return nil, err
	}
	journal := &mutationJournal{}
	if err := applyRemotePlaceholderPlan(planned, journal); err != nil {
		return nil, errors.Join(err, journal.rollback())
	}
	return journal.undo(), nil
}

func planRemotePlaceholdersAcross(
	changes []placeholderChange,
	frontmatter string,
) ([]placeholderMutation, error) {
	var planned []placeholderMutation
	for _, change := range changes {
		changePlan, err := planRemotePlaceholdersAt(
			change.base,
			change.name,
			frontmatter,
			change.enabled,
		)
		if err != nil {
			return nil, err
		}
		planned = append(planned, changePlan...)
	}
	return planned, nil
}

func (m *manager) changeRemotePlaceholdersAcross(
	changes []placeholderChange,
	frontmatter string,
) (func() error, error) {
	planned, err := planRemotePlaceholdersAcross(changes, frontmatter)
	if err != nil {
		return nil, err
	}
	journal := &mutationJournal{}
	if err := applyRemotePlaceholderPlan(planned, journal); err != nil {
		return nil, errors.Join(err, journal.rollback())
	}
	return journal.undo(), nil
}
