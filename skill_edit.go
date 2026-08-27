package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"path/filepath"

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
	newName := edited.Name
	var undoSelection func() error
	if newName != "" && newName != oldName {
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

		var previousLock, renamedLock lock
		renamedSelection := false
		err = m.updateSelectionLock(project, func(value *lock) (bool, error) {
			previousLock = value.clone()
			oldEnabled, exists := value.enabled(oldName)
			if m.global {
				if !exists {
					return false, nil
				}
				newEnabled, newExists := value.enabled(newName)
				switch {
				case !newExists:
					value.setEnabled(newName, oldEnabled)
				default:
					merged, err := mergeEnabled(oldEnabled, newEnabled)
					if err != nil {
						return false, err
					}
					value.setEnabled(newName, merged)
				}
				value.deleteEnabled(oldName)
				renamedLock = value.clone()
				renamedSelection = true
				return true, nil
			}

			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return false, err
			}
			effective := oldEnabled
			effectiveExists := exists
			if !effectiveExists {
				effective, effectiveExists = global.enabled(oldName)
			}
			if !effectiveExists ||
				(!exists && effective.Boolean != nil && !*effective.Boolean) {
				return false, nil
			}
			newEnabled, newExists := value.enabled(newName)
			switch {
			case !newExists:
				value.setEnabled(newName, effective)
			default:
				merged, err := mergeEnabled(effective, newEnabled)
				if err != nil {
					return false, err
				}
				value.setEnabled(newName, merged)
			}
			if _, inherited := global.enabled(oldName); inherited {
				value.setEnabled(oldName, enabledValue{Boolean: new(false)})
			} else {
				value.deleteEnabled(oldName)
			}
			renamedLock = value.clone()
			renamedSelection = true
			return true, nil
		})
		if err != nil {
			return nil, selectionState{}, err
		}
		if renamedSelection {
			undoSelection = func() error {
				return m.updateSelectionLock(project, func(value *lock) (bool, error) {
					if !value.equal(renamedLock) {
						return false, fmt.Errorf("selection changed during managed edit rollback")
					}
					*value = previousLock.clone()
					return true, nil
				})
			}
		}
	}
	rollbackSelection := func(cause error) error {
		if undoSelection == nil {
			return cause
		}
		return errors.Join(cause, undoSelection())
	}
	selection, err := m.selectionState(project, skills)
	if err != nil || edited.Source != managedSkillSource {
		if err != nil {
			err = rollbackSelection(err)
		}
		return skills, selection, err
	}
	frontmatter, err := m.placeholderFrontmatter(edited, nil)
	if err != nil {
		return skills, selection, rollbackSelection(err)
	}
	var changes []placeholderChange
	if m.global {
		global, err := loadLock(m.paths.globalLockDir)
		if err != nil {
			return skills, selection, rollbackSelection(err)
		}
		if oldName != newName {
			changes = append(changes, placeholderChange{base: m.paths.placeholderDir, name: oldName})
		}
		changes = append(changes, placeholderChange{
			base:    m.paths.placeholderDir,
			name:    newName,
			enabled: lockWantsPlaceholder(global, newName),
		})
	} else {
		projectLock, err := loadLock(project)
		if err != nil {
			return skills, selection, rollbackSelection(err)
		}
		sameRoot := filepath.Clean(project) == filepath.Clean(m.paths.placeholderDir)
		if sameRoot || oldName == newName {
			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return skills, selection, rollbackSelection(err)
			}
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
	}
	_, err = m.changeRemotePlaceholdersAcross(changes, frontmatter)
	if err != nil {
		err = rollbackSelection(err)
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

	var undoPlaceholder func() error
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
		undoPlaceholder, err = m.changeRemotePlaceholders(
			project,
			skill.Name,
			frontmatter,
			create,
		)
		if err != nil {
			return selectionState{}, err
		}
	}

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
		if desiredExists {
			value.setEnabled(skill.Name, desired)
		} else {
			value.deleteEnabled(skill.Name)
		}
		return true, nil
	})
	if err != nil {
		if undoPlaceholder != nil {
			err = errors.Join(err, undoPlaceholder())
		}
		return selectionState{}, err
	}
	return m.selectionState(project, nil)
}
