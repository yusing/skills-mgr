package main

import (
	"context"

	"errors"
	"fmt"
	"io"

	"maps"

	"slices"
)

type remoteToggleResult struct {
	Skill          string
	Enabled        bool
	Skills         []discoveredSkill
	Selected       map[string]bool
	RemoteSelected map[string]bool
}

type remoteUninstallResult struct {
	Skill           string
	Skills          []discoveredSkill
	Selected        map[string]bool
	GlobalSelected  map[string]bool
	ProjectSelected map[string]bool
}

func (m *manager) sync(
	ctx context.Context,
	project string,
	output io.Writer,
) (retErr error) {
	if m.remoteStore == nil {
		return fmt.Errorf("remote skill store is unavailable")
	}
	var journal mutationJournal
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, journal.rollback())
		}
	}()
	changePlaceholders := func(name, frontmatter string, enabled bool) error {
		undo, err := m.changeRemotePlaceholders(project, name, frontmatter, enabled)
		if err != nil {
			return err
		}
		journal.add(undo)
		return nil
	}

	globalLock, err := loadLock(m.paths.globalLockDir)
	if err != nil {
		return err
	}
	projectLock, err := loadLock(project)
	if err != nil {
		return err
	}
	originalLock := projectLock.clone()
	persistedRefs, err := m.persistedRemoteRefs()
	if err != nil {
		return err
	}
	for name, selection := range globalLock.Skills {
		if selection.Remote == nil {
			continue
		}
		ref := *selection.Remote
		if persisted, exists := persistedRefs[name]; exists && persisted != ref {
			return fmt.Errorf(
				"remote skill %q identity conflicts with persisted reference",
				name,
			)
		}
		persistedRefs[name] = ref
	}
	metadataChanged, err := reconcileRemoteMetadata(
		globalLock,
		&projectLock,
		persistedRefs,
	)
	if err != nil {
		return fmt.Errorf("reconcile remote metadata: %w", err)
	}

	selected, expressions := mergeSelectionLocks(globalLock, projectLock)
	selection := selectionState{selected: selected, expressions: expressions}
	evaluator := newEnabledEvaluator(project)
	discovered, err := m.skills(project)
	if err != nil {
		return err
	}
	discoveredByName := make(map[string]discoveredSkill, len(discovered))
	for _, skill := range discovered {
		discoveredByName[skill.Name] = skill
	}
	names := make([]string, 0, len(projectLock.Skills))
	for name, selection := range projectLock.Skills {
		if selection.Remote != nil {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	storeRecords, err := m.remoteStore.records()
	if err != nil {
		return err
	}
	recordsByKey := make(map[string]remoteSkillRecord, len(storeRecords))
	for _, record := range storeRecords {
		recordsByKey[record.ref().key()] = record
	}

	for _, name := range names {
		ref, _ := projectLock.remote(name)
		wantsPlaceholder := lockWantsPlaceholder(projectLock, name)
		if !wantsPlaceholder {
			// A missing or unreadable record leaves the frontmatter empty, which
			// is what removing a placeholder needs anyway.
			frontmatter := ""
			if record, ok := recordsByKey[ref.key()]; ok {
				frontmatter, _ = m.recordFrontmatter(record, ref)
			}
			if err := changePlaceholders(name, frontmatter, false); err != nil {
				return fmt.Errorf("sync remote skill %q: %w", name, err)
			}
		}
		effectivelyEnabled, err := selection.enabled(ctx, evaluator, name)
		if err != nil {
			return fmt.Errorf("sync remote skill %q: %w", name, err)
		}
		if !effectivelyEnabled {
			continue
		}
		if ref.Name != name {
			return fmt.Errorf(
				"sync remote skill %q: selection name does not match remote name %q",
				name,
				ref.Name,
			)
		}
		if skill, ok := discoveredByName[name]; ok && skill.RemoteKey != ref.key() {
			persisted, isPersisted := persistedRefs[name]
			if !isPersisted || persisted != ref {
				return fmt.Errorf(
					"sync remote skill %q: skill name is already discovered from %s",
					name,
					skill.Source,
				)
			}
		}
		if err := ref.validate(); err != nil {
			return fmt.Errorf("sync remote skill %q: %w", name, err)
		}
		provider := m.remoteContentProvider(ref.Provider)
		record, err := m.remoteStore.ensure(ctx, ref, provider)
		if err != nil {
			return fmt.Errorf("sync remote skill %q: %w", name, err)
		}
		frontmatter, err := m.recordFrontmatter(record, ref)
		if err != nil {
			return fmt.Errorf("sync remote skill %q: %w", name, err)
		}
		if wantsPlaceholder {
			if err := changePlaceholders(name, frontmatter, true); err != nil {
				return fmt.Errorf("sync remote skill %q: %w", name, err)
			}
		}
		if _, err := fmt.Fprintln(output, name); err != nil {
			return fmt.Errorf("report synchronized skill %q: %w", name, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reconcile remote metadata: %w", err)
	}
	err = updateLock(project, m.paths.selectionLocks, func(current *lock) (bool, error) {
		if !current.equal(originalLock) {
			return false, fmt.Errorf("project selection changed during sync")
		}
		if !metadataChanged {
			return false, nil
		}
		*current = projectLock.clone()
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("persist synchronized remote metadata: %w", err)
	}
	return nil
}

func (m *manager) remoteContentProvider(provider string) remoteSkillContentProvider {
	switch provider {
	case skillsShProvider:
		return m.remote
	case skillsMPProvider:
		return m.skillsMP
	default:
		return nil
	}
}

func remoteSelectionFrom(
	skills []discoveredSkill,
	selected map[string]bool,
) map[string]bool {
	remote := make(map[string]bool)
	for _, skill := range skills {
		if skill.RemoteKey != "" && selected[skill.Name] {
			remote[skill.RemoteKey] = true
		}
	}
	return remote
}

func (m *manager) toggleRemote(
	ctx context.Context,
	project string,
	ref remoteSkillRef,
) (remoteToggleResult, error) {
	if m.remoteStore == nil {
		return remoteToggleResult{}, fmt.Errorf("remote skill store is unavailable")
	}
	if err := ref.validate(); err != nil {
		return remoteToggleResult{}, err
	}
	selectionLock, err := loadLock(m.lockDir(project))
	if err != nil {
		return remoteToggleResult{}, err
	}
	previousEnabled, previousExists := selectionLock.enabled(ref.Name)
	previousRemote, previousRemoteExists := selectionLock.remote(ref.Name)
	rollbackSelection := func(expected bool) error {
		return m.updateSelectionLock(project, func(value *lock) (bool, error) {
			current, exists := value.enabled(ref.Name)
			currentRemote, remoteExists := value.remote(ref.Name)
			if !exists || current.Boolean == nil ||
				*current.Boolean != expected ||
				!remoteExists || currentRemote != ref {
				return false, fmt.Errorf("remote selection changed during placeholder rollback")
			}
			if previousExists {
				value.setEnabled(ref.Name, previousEnabled)
			} else {
				value.deleteEnabled(ref.Name)
			}
			if previousRemoteExists {
				value.setRemote(ref.Name, previousRemote)
			} else {
				value.deleteRemote(ref.Name)
			}
			return true, nil
		})
	}
	skills, err := m.skills(project)
	if err != nil {
		return remoteToggleResult{}, err
	}
	selection, err := m.selectionState(project, skills)
	if err != nil {
		return remoteToggleResult{}, err
	}
	selected := selection.selected
	key := ref.key()
	enabled := false
	for _, skill := range skills {
		if skill.Name != ref.Name {
			continue
		}
		if skill.RemoteKey != key {
			return remoteToggleResult{}, fmt.Errorf(
				"skill name %q is already discovered from %s",
				ref.Name,
				skill.Source,
			)
		}
		enabled = selected[skill.Name]
		break
	}
	if enabled {
		frontmatter, err := m.remoteSkillFrontmatter(ref)
		if err != nil {
			return remoteToggleResult{}, err
		}
		selected, err = m.setRemoteSelection(project, ref, false)
		if err != nil {
			return remoteToggleResult{}, err
		}
		if err := m.setRemotePlaceholders(project, ref.Name, frontmatter, false); err != nil {
			rollbackErr := rollbackSelection(false)
			return remoteToggleResult{}, errors.Join(err, rollbackErr)
		}
		return newRemoteToggleResult(ref.Name, false, skills, selected), nil
	}

	provider := m.remoteContentProvider(ref.Provider)
	if _, err := m.remoteStore.ensure(ctx, ref, provider); err != nil {
		return remoteToggleResult{}, err
	}
	skills, err = m.skills(project)
	if err != nil {
		return remoteToggleResult{}, err
	}
	found := false
	for _, skill := range skills {
		if skill.Name == ref.Name && skill.RemoteKey == key {
			found = true
			break
		}
	}
	if !found {
		return remoteToggleResult{}, fmt.Errorf(
			"persisted remote skill %q was not discovered",
			ref.Name,
		)
	}
	frontmatter, err := m.remoteSkillFrontmatter(ref)
	if err != nil {
		return remoteToggleResult{}, err
	}
	if err := m.setRemotePlaceholders(project, ref.Name, frontmatter, true); err != nil {
		return remoteToggleResult{}, err
	}
	selected, err = m.setRemoteSelection(project, ref, true)
	if err != nil {
		cleanupErr := m.setRemotePlaceholders(project, ref.Name, frontmatter, false)
		return remoteToggleResult{}, errors.Join(err, cleanupErr)
	}
	return newRemoteToggleResult(ref.Name, true, skills, selected), nil
}

func (m *manager) uninstallRemote(
	ctx context.Context,
	project string,
	name string,
	key string,
) (remoteUninstallResult, error) {
	if m.remoteStore == nil {
		return remoteUninstallResult{}, fmt.Errorf("remote skill store is unavailable")
	}
	ref, err := m.persistedRemoteRef(key, name)
	if err != nil {
		return remoteUninstallResult{}, err
	}
	if !m.global {
		global, err := loadLock(m.paths.globalLockDir)
		if err != nil {
			return remoteUninstallResult{}, err
		}
		_, globallyConfigured := global.remote(name)
		_, legacyGlobalSelection := global.enabled(name)
		if globallyConfigured ||
			(global.SchemaRevision == legacyLockSchemaRevision && legacyGlobalSelection) {
			return remoteUninstallResult{}, fmt.Errorf(
				"remote skill %q is configured globally; uninstall it from the global TUI",
				name,
			)
		}
	}
	frontmatter, err := m.remoteSkillFrontmatter(ref)
	if err != nil {
		return remoteUninstallResult{}, err
	}
	skills, err := m.discoverSkills(project, key)
	if err != nil {
		return remoteUninstallResult{}, err
	}

	var currentSelected map[string]bool
	var previousEnabled enabledValue
	var previousExists bool
	var previousRemote remoteSkillRef
	var previousRemoteExists bool
	err = m.updateSelectionLock(project, func(value *lock) (bool, error) {
		previousEnabled, previousExists = value.enabled(name)
		previousRemote, previousRemoteExists = value.remote(name)
		value.deleteEnabled(name)
		value.deleteRemote(name)
		currentSelected = configuredSelections(*value)
		return previousExists || previousRemoteExists, nil
	})
	if err != nil {
		return remoteUninstallResult{}, err
	}

	rollbackSelection := func() error {
		return m.updateSelectionLock(project, func(value *lock) (bool, error) {
			if _, exists := value.enabled(name); exists {
				return false, fmt.Errorf("remote selection changed during uninstall rollback")
			}
			if _, exists := value.remote(name); exists {
				return false, fmt.Errorf("remote selection changed during uninstall rollback")
			}
			if previousExists {
				value.setEnabled(name, previousEnabled)
			}
			if previousRemoteExists {
				value.setRemote(name, previousRemote)
			}
			return previousExists || previousRemoteExists, nil
		})
	}
	undoPlaceholders, err := m.changeRemotePlaceholders(project, name, frontmatter, false)
	if err != nil {
		return remoteUninstallResult{}, errors.Join(err, rollbackSelection())
	}
	rollbackPlaceholders := func() error {
		if undoPlaceholders == nil {
			return nil
		}
		return undoPlaceholders()
	}
	var globalSelected map[string]bool
	removeErr := updateLock(
		m.paths.globalLockDir,
		m.paths.selectionLocks,
		func(global *lock) (bool, error) {
			_, globallySelected := global.enabled(name)
			_, globallyConfigured := global.remote(name)
			legacyGlobalSelection := global.SchemaRevision == legacyLockSchemaRevision &&
				globallySelected
			if !m.global && (globallyConfigured || legacyGlobalSelection) {
				return false, fmt.Errorf(
					"remote skill %q is configured globally; uninstall it from the global TUI",
					name,
				)
			}
			if m.global && (globallySelected || globallyConfigured) {
				return false, fmt.Errorf("remote selection changed during uninstall")
			}
			globalSelected = configuredSelections(*global)
			return false, m.remoteStore.remove(ctx, ref)
		},
	)
	if removeErr != nil {
		return remoteUninstallResult{}, errors.Join(
			removeErr,
			rollbackSelection(),
			rollbackPlaceholders(),
		)
	}

	selected := maps.Clone(globalSelected)
	var resultGlobalSelected, projectSelected map[string]bool
	if !m.global {
		resultGlobalSelected = globalSelected
		projectSelected = currentSelected
		maps.Copy(selected, projectSelected)
		for _, skill := range skills {
			if skillEnabled(selected, skill) {
				selected[skill.Name] = true
			}
		}
	}
	return remoteUninstallResult{
		Skill:           name,
		Skills:          skills,
		Selected:        selected,
		GlobalSelected:  resultGlobalSelected,
		ProjectSelected: projectSelected,
	}, nil
}

func newRemoteToggleResult(
	skill string,
	enabled bool,
	skills []discoveredSkill,
	selected map[string]bool,
) remoteToggleResult {
	return remoteToggleResult{
		Skill:          skill,
		Enabled:        enabled,
		Skills:         skills,
		Selected:       selected,
		RemoteSelected: remoteSelectionFrom(skills, selected),
	}
}
