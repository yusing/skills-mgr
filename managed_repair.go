package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
)

// repairManagedPlaceholders repairs shared home placeholders independently of
// the invoking project's discovery precedence, then its project-owned stubs.
func (m *manager) repairManagedPlaceholders(ctx context.Context, project string, logger *slog.Logger) error {
	guard, err := m.lockManagedMutation(ctx)
	if err != nil {
		logger.Error("lock managed placeholder repair", "err", err)
		return err
	}
	defer closeExclusiveLock(guard)
	globalErr := m.repairManagedPlaceholderScope(ctx, m.paths.globalLockDir, m.paths.placeholderDir, true)
	if globalErr != nil {
		logger.Error("repair global managed placeholders", "err", globalErr)
	}
	atHome, err := m.paths.atHome(project)
	if err != nil {
		logger.Error("resolve managed placeholder repair root", "project", project, "err", err)
		return errors.Join(globalErr, err)
	}
	if atHome {
		return globalErr
	}
	projectErr := m.repairManagedPlaceholderScope(ctx, project, project, false)
	if projectErr != nil {
		logger.Error("repair project managed placeholders", "project", project, "err", projectErr)
	}
	return errors.Join(globalErr, projectErr)
}

// lockManagedMutation covers selection, content, placeholder changes and rollback
// together. The shorter selection-file lock alone cannot serialize those steps.
func (m *manager) lockManagedMutation(ctx context.Context) (*os.File, error) {
	if err := os.MkdirAll(m.paths.selectionLocks, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(m.paths.selectionLocks, "managed-mutation.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flockExclusiveContext(ctx, file, "managed mutation"); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (m *manager) repairManagedPlaceholderScope(ctx context.Context, selectionDir, base string, global bool) (retErr error) {
	var skills []discoveredSkill
	if global {
		discovery := newSkillDiscovery()
		if err := discovery.discoverRoot(skillRoot{path: m.paths.managedSkills, source: managedSkillSource, editable: true}); err != nil {
			return err
		}
		slices.SortFunc(discovery.skills, compareDiscoveredSkills)
		skills = discovery.skills
	} else {
		var err error
		skills, err = m.skills(base)
		if err != nil {
			return err
		}
	}
	globalLock, err := loadLock(m.paths.globalLockDir)
	if err != nil {
		return err
	}
	var journal mutationJournal
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, journal.rollback())
		}
	}()
	// Hold the selection lock while deriving and applying the placeholder plan.
	// Repair never changes selections or evaluates conditional expressions.
	return updateLock(selectionDir, m.paths.selectionLocks, func(value *lock) (bool, error) {
		for _, skill := range skills {
			if skill.Source != managedSkillSource || skill.RemoteKey != "" {
				continue
			}
			if _, remote := value.remote(skill.Name); remote {
				continue
			}
			if !global {
				if _, remote := globalLock.remote(skill.Name); remote {
					continue
				}
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			enabled := lockWantsPlaceholder(*value, skill.Name)
			frontmatter, err := m.placeholderFrontmatter(skill, nil)
			if err != nil && enabled {
				return false, fmt.Errorf("repair managed skill %q: %w", skill.Name, err)
			}
			undo, err := m.changeRemotePlaceholdersAcross([]placeholderChange{{base: base, name: skill.Name, enabled: enabled}}, frontmatter)
			if err != nil {
				return false, fmt.Errorf("repair managed skill %q: %w", skill.Name, err)
			}
			journal.add(undo)
		}
		return false, ctx.Err()
	})
}
