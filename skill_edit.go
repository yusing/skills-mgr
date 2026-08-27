package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"strings"
)

func (m *manager) completeSkillEdit(
	ctx context.Context,
	project string,
	edited discoveredSkill,
	draft string,
	layeredDigest [sha256.Size]byte,
) ([]discoveredSkill, selectionState, error) {
	if edited.RemoteKey == "" {
		return m.refreshEditedSkill(project, edited.Name, edited.Path)
	}
	if draft == "" {
		return nil, selectionState{}, fmt.Errorf("remote skill edit draft is missing")
	}
	contents, err := os.ReadFile(draft)
	if err != nil {
		return nil, selectionState{}, fmt.Errorf("read remote skill edit: %w", err)
	}
	ref, err := m.persistedRemoteRef(edited.RemoteKey, edited.Name)
	if err != nil {
		return nil, selectionState{}, err
	}
	if err := m.remoteStore.savePatch(
		ctx,
		ref,
		edited.Path,
		layeredDigest,
		contents,
	); err != nil {
		return nil, selectionState{}, err
	}
	skills, err := m.skills(project)
	if err != nil {
		return nil, selectionState{}, err
	}
	selection, err := m.selectionState(project, skills)
	return skills, selection, err
}

type selectionRenamePlan struct {
	before  lock
	after   lock
	changed bool
}

func (m *manager) planSelectionRename(
	project, oldName, newName string,
) (selectionRenamePlan, error) {
	current, err := loadLock(m.lockDir(project))
	if err != nil {
		return selectionRenamePlan{}, err
	}
	if _, err := m.migrateSelectionLock(&current); err != nil {
		return selectionRenamePlan{}, err
	}
	planned := selectionRenamePlan{
		before: current.clone(),
		after:  current.clone(),
	}
	if newName == "" || newName == oldName {
		return planned, nil
	}
	mergeEnabled := func(oldEnabled, newEnabled enabledValue) (enabledValue, error) {
		if oldEnabled.Boolean == nil || newEnabled.Boolean == nil {
			return enabledValue{}, fmt.Errorf(
				"cannot merge enabled values while renaming skill %q to %q when either value is an expression",
				oldName,
				newName,
			)
		}
		merged := *newEnabled.Boolean || *oldEnabled.Boolean
		return enabledValue{Boolean: new(merged)}, nil
	}

	value := &planned.after
	oldEnabled, exists := value.enabled(oldName)
	if m.global {
		if !exists {
			return planned, nil
		}
		newEnabled, newExists := value.enabled(newName)
		switch {
		case !newExists:
			value.setEnabled(newName, oldEnabled)
		default:
			merged, err := mergeEnabled(oldEnabled, newEnabled)
			if err != nil {
				return selectionRenamePlan{}, err
			}
			value.setEnabled(newName, merged)
		}
		value.deleteEnabled(oldName)
		planned.changed = true
		return planned, nil
	}

	global, err := loadLock(m.paths.globalLockDir)
	if err != nil {
		return selectionRenamePlan{}, err
	}
	effective := oldEnabled
	effectiveExists := exists
	if !effectiveExists {
		effective, effectiveExists = global.enabled(oldName)
	}
	if !effectiveExists ||
		(!exists && effective.Boolean != nil && !*effective.Boolean) {
		return planned, nil
	}
	newEnabled, newExists := value.enabled(newName)
	switch {
	case !newExists:
		value.setEnabled(newName, effective)
	default:
		merged, err := mergeEnabled(effective, newEnabled)
		if err != nil {
			return selectionRenamePlan{}, err
		}
		value.setEnabled(newName, merged)
	}
	if _, inherited := global.enabled(oldName); inherited {
		value.setEnabled(oldName, enabledValue{Boolean: new(false)})
	} else {
		value.deleteEnabled(oldName)
	}
	planned.changed = true
	return planned, nil
}

func (m *manager) applySelectionRename(
	project string,
	planned selectionRenamePlan,
	journal *mutationJournal,
) error {
	if !planned.changed {
		return nil
	}
	if err := m.updateSelectionLock(project, func(value *lock) (bool, error) {
		if !value.equal(planned.before) {
			return false, fmt.Errorf("selection changed during managed edit")
		}
		*value = planned.after.clone()
		return true, nil
	}); err != nil {
		return err
	}
	journal.add(func() error {
		return m.updateSelectionLock(project, func(value *lock) (bool, error) {
			if !value.equal(planned.after) {
				return false, fmt.Errorf("selection changed during managed edit rollback")
			}
			*value = planned.before.clone()
			return true, nil
		})
	})
	return nil
}

func (m *manager) planEditedSkillPlaceholders(
	project, oldName string,
	edited discoveredSkill,
	renamed selectionRenamePlan,
) ([]placeholderMutation, error) {
	if edited.Source != managedSkillSource {
		return nil, nil
	}
	newName := edited.Name
	frontmatter, err := m.placeholderFrontmatter(edited, nil)
	if err != nil {
		return nil, err
	}
	var changes []placeholderChange
	if m.global {
		global := renamed.after
		if oldName != newName {
			changes = append(changes, placeholderChange{
				base: m.paths.placeholderDir,
				name: oldName,
			})
		}
		changes = append(changes, placeholderChange{
			base:    m.paths.placeholderDir,
			name:    newName,
			enabled: lockWantsPlaceholder(global, newName),
		})
		return planRemotePlaceholdersAcross(changes, frontmatter)
	}

	projectLock := renamed.after
	global, err := loadLock(m.paths.globalLockDir)
	if err != nil {
		return nil, err
	}
	sameRoot, err := samePlaceholderRoot(project, m.paths.placeholderDir)
	if err != nil {
		return nil, err
	}
	if sameRoot || oldName == newName {
		if sameRoot && oldName != newName {
			changes = append(changes,
				placeholderChange{
					base: project,
					name: oldName,
					enabled: lockWantsPlaceholder(global, oldName) ||
						lockWantsPlaceholder(projectLock, oldName),
				},
				placeholderChange{
					base: project,
					name: newName,
					enabled: lockWantsPlaceholder(global, newName) ||
						lockWantsPlaceholder(projectLock, newName),
				},
			)
		} else {
			enabled := lockWantsPlaceholder(global, newName)
			if sameRoot {
				enabled = enabled || lockWantsPlaceholder(projectLock, newName)
			}
			changes = append(changes, placeholderChange{
				base:    m.paths.placeholderDir,
				name:    newName,
				enabled: enabled,
			})
		}
	}
	if !sameRoot {
		if oldName != newName {
			changes = append(changes, placeholderChange{base: project, name: oldName})
		}
		changes = append(changes, placeholderChange{
			base:    project,
			name:    newName,
			enabled: lockWantsPlaceholder(projectLock, newName),
		})
	}
	return planRemotePlaceholdersAcross(changes, frontmatter)
}

func (m *manager) refreshEditedSkill(
	project string,
	oldName string,
	path string,
) ([]discoveredSkill, selectionState, error) {
	skills, err := m.skills(project)
	if err != nil {
		return nil, selectionState{}, err
	}
	var edited discoveredSkill
	for _, skill := range skills {
		if skill.Path == path {
			edited = skill
			break
		}
	}
	renamed, err := m.planSelectionRename(project, oldName, edited.Name)
	if err != nil {
		return nil, selectionState{}, err
	}
	placeholderPlan, err := m.planEditedSkillPlaceholders(
		project,
		oldName,
		edited,
		renamed,
	)
	if err != nil {
		return skills, selectionState{}, err
	}

	journal := &mutationJournal{}
	if err := m.applySelectionRename(project, renamed, journal); err != nil {
		return nil, selectionState{}, err
	}
	if err := applyRemotePlaceholderPlan(placeholderPlan, journal); err != nil {
		return skills, selectionState{}, errors.Join(err, journal.rollback())
	}
	selection, err := m.selectionState(project, skills)
	if err != nil {
		err = errors.Join(err, journal.rollback())
	}
	return skills, selection, err
}

func (m *manager) applyEnabledDraft(
	project string,
	skill discoveredSkill,
	draft string,
) (selectionState, error) {
	defer os.Remove(draft)
	data, err := os.ReadFile(draft)
	if err != nil {
		return selectionState{}, fmt.Errorf("read enabled editor draft: %w", err)
	}
	var desired enabledValue
	draftValue := strings.TrimSpace(string(data))
	desiredExists := draftValue != ""
	if desiredExists {
		switch {
		case draftValue == "true" || draftValue == "false" || strings.HasPrefix(draftValue, `"`):
			if err := json.Unmarshal([]byte(draftValue), &desired); err != nil {
				return selectionState{}, fmt.Errorf("decode enabled editor draft: %w", err)
			}
		default:
			desired.Expression = draftValue
		}
	}

	var placeholderPlan []placeholderMutation
	if placeholderManaged(skill) {
		var ref *remoteSkillRef
		if skill.RemoteKey != "" {
			resolved, err := m.persistedRemoteRef(skill.RemoteKey, skill.Name)
			if err != nil {
				return selectionState{}, err
			}
			ref = &resolved
		}
		frontmatter, err := m.placeholderFrontmatter(skill, ref)
		if err != nil {
			return selectionState{}, err
		}
		effective, effectiveExists := desired, desiredExists
		if !desiredExists && !m.global {
			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return selectionState{}, err
			}
			effective, effectiveExists = global.enabled(skill.Name)
		}
		// A conditional entry still gets a placeholder. The stub is invisible to
		// the model, so it cannot bypass a false condition; only list, get, and
		// run answer for the model, and all three evaluate the expression.
		create := effectiveExists &&
			(effective.Boolean == nil || *effective.Boolean)
		base := project
		if m.global {
			base = m.paths.placeholderDir
		}
		placeholderPlan, err = planRemotePlaceholdersAt(
			base,
			skill.Name,
			frontmatter,
			create,
		)
		if err != nil {
			return selectionState{}, err
		}
	}

	journal := &mutationJournal{}
	if err := applyRemotePlaceholderPlan(placeholderPlan, journal); err != nil {
		return selectionState{}, errors.Join(err, journal.rollback())
	}
	var before, after lock
	selectionChanged := false
	err = m.updateSelectionLock(project, func(value *lock) (bool, error) {
		current, currentExists := value.enabled(skill.Name)
		same := currentExists == desiredExists
		if same && currentExists {
			same = current.Expression == desired.Expression &&
				((current.Boolean == nil && desired.Boolean == nil) ||
					(current.Boolean != nil &&
						desired.Boolean != nil &&
						*current.Boolean == *desired.Boolean))
		}
		if same {
			return false, nil
		}
		before = value.clone()
		if desiredExists {
			value.setEnabled(skill.Name, desired)
		} else {
			value.deleteEnabled(skill.Name)
		}
		after = value.clone()
		selectionChanged = true
		return true, nil
	})
	if err != nil {
		return selectionState{}, errors.Join(err, journal.rollback())
	}
	if selectionChanged {
		journal.add(func() error {
			return m.updateSelectionLock(project, func(value *lock) (bool, error) {
				if !value.equal(after) {
					return false, fmt.Errorf("selection changed during enabled edit rollback")
				}
				*value = before.clone()
				return true, nil
			})
		})
	}
	selection, err := m.selectionState(project, nil)
	if err != nil {
		err = errors.Join(err, journal.rollback())
	}
	return selection, err
}
