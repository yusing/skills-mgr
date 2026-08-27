package main

import (
	"errors"
	"fmt"
	"io"

	"os"

	"path/filepath"
	"slices"

	"strings"
)

// placeholderManaged reports whether skills-mgr owns a skill's content, and so
// whether a harness needs a placeholder to offer its name. Content already
// sitting in a placeholder root is left alone: the harness can see it without
// help, and writing a stub there would collide with the real directory.
func placeholderManaged(skill discoveredSkill) bool {
	return skill.RemoteKey != "" || skill.Source == managedSkillSource
}

// placeholderFrontmatter resolves the frontmatter a placeholder should carry. A
// remote skill reads from the store, so the stub matches the persisted identity
// rather than whatever is on disk; anything else reads its own SKILL.md.
func (m *manager) placeholderFrontmatter(
	skill discoveredSkill,
	remoteRef *remoteSkillRef,
) (string, error) {
	if remoteRef != nil {
		return m.remoteSkillFrontmatter(*remoteRef)
	}
	file, err := os.Open(skill.Path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", skill.Path, err)
	}
	defer file.Close()
	frontmatter, _, status, err := readFrontmatter(file)
	if err != nil {
		return "", fmt.Errorf("read frontmatter from %s: %w", skill.Path, err)
	}
	if status != frontmatterValid {
		return "", fmt.Errorf("%s has no valid frontmatter", skill.Path)
	}
	return frontmatter, nil
}

func lockWantsPlaceholder(value lock, name string) bool {
	enabled, exists := value.enabled(name)
	return exists && (enabled.Boolean == nil || *enabled.Boolean)
}

// adoptSkill moves a user skill into the manager home, where no harness scans
// it, and leaves a placeholder behind when the skill is enabled here. Adoption
// is what turns an always-loaded skill into one the selection governs.
func (m *manager) adoptSkill(project string, skill discoveredSkill) (func() error, error) {
	if skill.Source != "user" {
		return nil, fmt.Errorf("only a skill under %s can be adopted", m.paths.userSkills)
	}
	if err := validateRelocationEntry(skill); err != nil {
		return nil, err
	}
	frontmatter, err := m.placeholderFrontmatter(skill, nil)
	if err != nil {
		return nil, err
	}
	global, err := loadLock(m.paths.globalLockDir)
	if err != nil {
		return nil, err
	}
	changes := []placeholderChange{{
		base:    m.paths.placeholderDir,
		name:    skill.Name,
		enabled: lockWantsPlaceholder(global, skill.Name),
	}}
	if !m.global {
		projectLock, err := loadLock(project)
		if err != nil {
			return nil, err
		}
		projectWantsPlaceholder := lockWantsPlaceholder(projectLock, skill.Name)
		sameRoot, err := samePlaceholderRoot(project, m.paths.placeholderDir)
		if err != nil {
			return nil, err
		}
		if sameRoot {
			changes[0].enabled = changes[0].enabled || projectWantsPlaceholder
		} else {
			changes = append(changes, placeholderChange{
				base:    project,
				name:    skill.Name,
				enabled: projectWantsPlaceholder,
			})
		}
	}

	destination := filepath.Join(m.paths.managedSkills, skill.Name)
	if err := moveSkillDirectory(skill.EntryPath, destination); err != nil {
		return nil, err
	}
	undoPlaceholders, err := m.changeRemotePlaceholdersAcross(changes, frontmatter)
	if err != nil {
		moveErr := moveSkillDirectory(destination, skill.EntryPath)
		return nil, errors.Join(err, moveErr)
	}
	return func() error {
		placeholderErr := error(nil)
		if undoPlaceholders != nil {
			placeholderErr = undoPlaceholders()
		}
		moveErr := moveSkillDirectory(destination, skill.EntryPath)
		return errors.Join(placeholderErr, moveErr)
	}, nil
}

// adoptSharedSkills moves every real skill in the shared user root as one
// transaction. Direct discovery keeps a same-named project skill from hiding
// shared content that the command was asked to adopt.
func (m *manager) adoptSharedSkills(project string, output io.Writer) (retErr error) {
	discovery := skillDiscovery{
		seenPaths: make(map[string]struct{}),
		seenNames: make(map[string]struct{}),
	}
	if err := discovery.discoverRoot(skillRoot{
		path:     m.paths.userSkills,
		source:   "user",
		editable: true,
	}); err != nil {
		return err
	}
	slices.SortFunc(discovery.skills, func(a, b discoveredSkill) int {
		return strings.Compare(a.Name, b.Name)
	})

	journal := &mutationJournal{}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, journal.rollback())
		}
	}()

	var report strings.Builder
	for _, skill := range discovery.skills {
		undo, err := m.adoptSkill(project, skill)
		if err != nil {
			return fmt.Errorf("adopt skill %q: %w", skill.Name, err)
		}
		journal.add(undo)
		report.WriteString(skill.Name)
		report.WriteByte('\n')
	}
	if report.Len() == 0 {
		return nil
	}
	if _, err := io.WriteString(output, report.String()); err != nil {
		return fmt.Errorf("report adopted skills: %w", err)
	}
	return nil
}

// releaseSkill moves a managed skill back under $HOME/.agents/skills, where
// every harness loads it again regardless of the selection. Placeholders come
// off first: in global mode the destination is itself a placeholder path, so a
// surviving stub would block the move.
func (m *manager) releaseSkill(project string, skill discoveredSkill) (func() error, error) {
	if skill.Source != managedSkillSource {
		return nil, fmt.Errorf("only a skill in the manager home can be released")
	}
	if err := validateRelocationEntry(skill); err != nil {
		return nil, err
	}
	frontmatter, err := m.placeholderFrontmatter(skill, nil)
	if err != nil {
		return nil, err
	}
	changes := []placeholderChange{{base: m.paths.placeholderDir, name: skill.Name}}
	sameRoot, err := samePlaceholderRoot(project, m.paths.placeholderDir)
	if err != nil {
		return nil, err
	}
	if !sameRoot {
		changes = append(changes, placeholderChange{base: project, name: skill.Name})
	}
	undoPlaceholders, err := m.changeRemotePlaceholdersAcross(changes, frontmatter)
	if err != nil {
		return nil, err
	}
	destination := filepath.Join(m.paths.userSkills, skill.Name)
	if err := moveSkillDirectory(skill.EntryPath, destination); err != nil {
		if undoPlaceholders != nil {
			err = errors.Join(err, undoPlaceholders())
		}
		return nil, err
	}
	return func() error {
		if err := moveSkillDirectory(destination, skill.EntryPath); err != nil {
			return err
		}
		if undoPlaceholders == nil {
			return nil
		}
		return undoPlaceholders()
	}, nil
}

// relocateSkill moves the selected skill between $HOME/.agents/skills and the
// manager home, so one keystroke decides whether every harness always loads the
// skill or the selection governs it.
func (m *manager) relocateSkill(
	project string,
	skill discoveredSkill,
) (skillLocationResult, error) {
	adopt := skill.Source != managedSkillSource
	var undo func() error
	var err error
	if adopt {
		undo, err = m.adoptSkill(project, skill)
	} else {
		undo, err = m.releaseSkill(project, skill)
	}
	if err != nil {
		return skillLocationResult{}, err
	}
	skills, err := m.skills(project)
	if err != nil {
		return skillLocationResult{}, errors.Join(err, undo())
	}
	selection, err := m.selectionState(project, skills)
	if err != nil {
		return skillLocationResult{}, errors.Join(err, undo())
	}
	return skillLocationResult{
		Skill:           skill.Name,
		Managed:         adopt,
		Skills:          skills,
		Selected:        selection.selected,
		GlobalSelected:  selection.globalSelected,
		ProjectSelected: selection.projectSelected,
	}, nil
}

func validateRelocationEntry(skill discoveredSkill) error {
	if skill.EntryPath == "" {
		return fmt.Errorf("skill %q has no relocatable directory entry", skill.Name)
	}
	resolved, err := filepath.EvalSymlinks(skill.EntryPath)
	if err != nil {
		return fmt.Errorf("resolve skill directory %s: %w", skill.EntryPath, err)
	}
	if filepath.Clean(resolved) != filepath.Clean(skill.Root) {
		return fmt.Errorf("skill directory %s changed before relocation", skill.EntryPath)
	}
	if filepath.Clean(skill.EntryPath) != filepath.Clean(resolved) {
		return fmt.Errorf("skill directory %s contains a symbolic link", skill.EntryPath)
	}
	return nil
}

func moveSkillDirectory(source, destination string) error {
	parent := filepath.Dir(destination)
	if err := validateRelocationParent(parent); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("%s already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", destination, err)
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", parent, err)
	}
	if err := validateRelocationParent(parent); err != nil {
		return err
	}
	if err := os.Rename(source, destination); err != nil {
		return fmt.Errorf("move %s to %s: %w", source, destination, err)
	}
	return nil
}

func validateRelocationParent(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve relocation destination parent %s: %w", path, err)
	}
	volume := filepath.VolumeName(absolute)
	current := volume + string(filepath.Separator)
	relative := strings.TrimPrefix(absolute, current)
	for component := range strings.SplitSeq(filepath.ToSlash(relative), "/") {
		if component == "" {
			continue
		}
		current = filepath.Join(current, filepath.FromSlash(component))
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect relocation destination parent %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("relocation destination parent %s contains a symbolic link", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("relocation destination parent %s is not a directory", current)
		}
	}
	return nil
}

func remoteRefUnchanged(value lock, skill string, ref *remoteSkillRef) bool {
	current, exists := value.remote(skill)
	if ref == nil {
		return !exists
	}
	return exists && current == *ref
}
