package main

import (
	"context"

	"errors"
	"fmt"
	"io"

	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

type selectionState struct {
	selected           map[string]bool
	globalSelected     map[string]bool
	projectSelected    map[string]bool
	expressions        map[string]string
	globalExpressions  map[string]string
	projectExpressions map[string]string
}

func (s selectionState) enabled(
	ctx context.Context,
	evaluator *enabledEvaluator,
	name string,
) (bool, error) {
	if !s.selected[name] {
		return false, nil
	}
	expression, conditional := s.expressions[name]
	if !conditional {
		return true, nil
	}
	return evaluator.evaluate(ctx, name, expression)
}

func (m *manager) selectionState(
	project string,
	catalog []discoveredSkill,
) (selectionState, error) {
	global, err := loadLock(m.paths.globalLockDir)
	if err != nil {
		return selectionState{}, err
	}
	if m.global {
		selected := configuredSelections(global)
		return selectionState{
			selected:          selected,
			expressions:       selectionExpressions(global),
			globalExpressions: selectionExpressions(global),
		}, nil
	}
	projectLock, err := loadLock(project)
	if err != nil {
		return selectionState{}, err
	}
	selected, expressions := mergeSelectionLocks(global, projectLock)
	var skills []discoveredSkill
	if catalog == nil {
		skills, err = m.skills(project)
		if err != nil {
			return selectionState{}, err
		}
	} else {
		skills = catalog
	}
	for _, skill := range skills {
		if skillEnabled(selected, skill) {
			selected[skill.Name] = true
		}
	}
	return selectionState{
		selected:           selected,
		globalSelected:     configuredSelections(global),
		projectSelected:    configuredSelections(projectLock),
		expressions:        expressions,
		globalExpressions:  selectionExpressions(global),
		projectExpressions: selectionExpressions(projectLock),
	}, nil
}

func (m *manager) lockDir(project string) string {
	if m.global {
		return m.paths.globalLockDir
	}
	return project
}

func (m *manager) toggle(project, skill string, remoteKey ...string) (bool, error) {
	var remoteRef *remoteSkillRef
	if len(remoteKey) > 0 && remoteKey[0] != "" {
		ref, err := m.persistedRemoteRef(remoteKey[0], skill)
		if err != nil {
			return false, err
		}
		remoteRef = new(ref)
	}

	catalog, err := m.skills(project)
	if err != nil {
		return false, err
	}
	var found discoveredSkill
	for _, discovered := range catalog {
		if discovered.Name == skill {
			found = discovered
			break
		}
	}
	// The global layer resolves against the name alone, so a project-scoped or
	// user-invocable-only skill does not read as already enabled there.
	target := discoveredSkill{Name: skill}
	if !m.global {
		target = found
		if target.Name == "" {
			target = discoveredSkill{Name: skill}
		}
	}

	enabled := false
	var previousEnabled enabledValue
	previousExists := false
	var previousRemote remoteSkillRef
	previousRemoteExists := false
	err = m.updateSelectionLock(project, func(value *lock) (bool, error) {
		selected := configuredSelections(*value)
		if !m.global {
			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return false, err
			}
			selected, _ = mergeSelectionLocks(global, *value)
		}
		enabled = !skillEnabled(selected, target)
		if enabled && m.global && remoteRef != nil {
			if err := m.validatePersistedRemoteRef(*remoteRef); err != nil {
				return false, err
			}
		}
		previousEnabled, previousExists = value.enabled(skill)
		previousRemote, previousRemoteExists = value.remote(skill)
		value.setEnabled(skill, enabledValue{Boolean: new(enabled)})
		if remoteRef == nil {
			value.deleteRemote(skill)
		} else {
			value.setRemote(skill, *remoteRef)
		}

		return true, nil
	})
	if err != nil {
		return false, err
	}
	if !placeholderManaged(found) {
		return enabled, nil
	}
	frontmatter, placeholderErr := m.placeholderFrontmatter(found, remoteRef)
	if placeholderErr == nil {
		placeholderErr = m.setRemotePlaceholders(project, skill, frontmatter, enabled)
	}
	if placeholderErr != nil {
		rollbackErr := m.updateSelectionLock(project, func(value *lock) (bool, error) {
			current, exists := value.enabled(skill)
			if !exists || current.Boolean == nil ||
				*current.Boolean != enabled ||
				!remoteRefUnchanged(*value, skill, remoteRef) {
				return false, fmt.Errorf("remote selection changed during placeholder rollback")
			}
			if previousExists {
				value.setEnabled(skill, previousEnabled)
			} else {
				value.deleteEnabled(skill)
			}
			if previousRemoteExists {
				value.setRemote(skill, previousRemote)
			} else {
				value.deleteRemote(skill)
			}
			return true, nil
		})
		return false, errors.Join(placeholderErr, rollbackErr)
	}
	return enabled, nil
}

func (m *manager) setRemoteSelection(
	project string,
	ref remoteSkillRef,
	enabled bool,
) (map[string]bool, error) {
	var selected map[string]bool
	err := m.updateSelectionLock(project, func(value *lock) (bool, error) {
		if enabled && m.global {
			if err := m.validatePersistedRemoteRef(ref); err != nil {
				return false, err
			}
		}
		value.setEnabled(ref.Name, enabledValue{Boolean: new(enabled)})
		value.setRemote(ref.Name, ref)
		selected = configuredSelections(*value)
		if !m.global {
			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return false, err
			}
			selected, _ = mergeSelectionLocks(global, *value)
		}
		return true, nil
	})
	return selected, err
}

func (m *manager) updateSelectionLock(
	project string,
	update func(*lock) (bool, error),
) error {
	lockDir := m.lockDir(project)
	updateMigrated := func(value *lock) (bool, error) {
		migrated, err := m.migrateSelectionLock(value)
		if err != nil {
			return false, err
		}
		changed, err := update(value)
		return migrated || changed, err
	}
	return updateLock(lockDir, m.paths.selectionLocks, updateMigrated)
}

func (m *manager) migrateSelectionLock(value *lock) (bool, error) {
	if value.SchemaRevision == lockSchemaRevision {
		return false, nil
	}
	if value.SchemaRevision == previousLockSchemaRevision {
		value.SchemaRevision = lockSchemaRevision
		return true, nil
	}
	refs, err := m.persistedRemoteRefs()
	if err != nil {
		return false, err
	}
	global := newLock()
	if !m.global {
		global, err = loadLock(m.paths.globalLockDir)
		if err != nil {
			return false, err
		}
	}
	if _, err := reconcileRemoteMetadata(global, value, refs); err != nil {
		return false, err
	}
	value.SchemaRevision = lockSchemaRevision
	return true, nil
}

func (m *manager) persistedRemoteRefs() (map[string]remoteSkillRef, error) {
	if m.remoteStore == nil {
		return make(map[string]remoteSkillRef), nil
	}
	records, err := m.remoteStore.records()
	if err != nil {
		return nil, err
	}
	refs := make(map[string]remoteSkillRef, len(records))
	for _, record := range records {
		ref := record.ref()
		if existing, ok := refs[ref.Name]; ok && existing != ref {
			return nil, fmt.Errorf(
				"multiple persisted remote identities for skill %q",
				ref.Name,
			)
		}
		refs[ref.Name] = ref
	}
	return refs, nil
}

func (m *manager) persistedRemoteRef(
	key string,
	name string,
) (remoteSkillRef, error) {
	if m.remoteStore == nil {
		return remoteSkillRef{}, fmt.Errorf("remote skill store is unavailable")
	}
	records, err := m.remoteStore.records()
	if err != nil {
		return remoteSkillRef{}, err
	}
	for _, record := range records {
		ref := record.ref()
		if ref.key() != key {
			continue
		}
		if ref.Name != name {
			return remoteSkillRef{}, fmt.Errorf(
				"remote skill metadata for %q belongs to skill %q",
				key,
				ref.Name,
			)
		}
		return ref, nil
	}
	return remoteSkillRef{}, fmt.Errorf(
		"remote skill metadata for %q is unavailable",
		name,
	)
}

func (m *manager) validatePersistedRemoteRef(expected remoteSkillRef) error {
	current, err := m.persistedRemoteRef(expected.key(), expected.Name)
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("persisted remote skill identity changed")
	}
	return nil
}

func reconcileRemoteMetadata(
	globalLock lock,
	projectLock *lock,
	refs map[string]remoteSkillRef,
) (bool, error) {
	changed := false
	add := func(name string) error {
		ref, ok := refs[name]
		if !ok {
			return nil
		}
		if existing, ok := projectLock.remote(name); ok {
			if existing != ref {
				return fmt.Errorf(
					"remote skill %q identity conflicts with persisted reference",
					name,
				)
			}
			return nil
		}
		projectLock.setRemote(name, ref)
		changed = true
		return nil
	}
	for name, selection := range projectLock.Skills {
		if selection.Enabled == nil {
			continue
		}
		if err := add(name); err != nil {
			return false, err
		}
	}
	for name, selection := range globalLock.Skills {
		if selection.Enabled == nil {
			continue
		}
		if _, overridden := projectLock.enabled(name); overridden {
			continue
		}
		if err := add(name); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func configuredSelections(value lock) map[string]bool {
	selected := make(map[string]bool, len(value.Skills))
	for name, selection := range value.Skills {
		if selection.Enabled == nil {
			continue
		}
		if selection.Enabled.Boolean == nil {
			selected[name] = true
			continue
		}
		selected[name] = *selection.Enabled.Boolean
	}
	return selected
}

func selectionExpressions(value lock) map[string]string {
	expressions := make(map[string]string)
	for name, selection := range value.Skills {
		if selection.Enabled != nil && selection.Enabled.Boolean == nil {
			expressions[name] = selection.Enabled.Expression
		}
	}
	return expressions
}

func mergeSelectionLocks(global, project lock) (map[string]bool, map[string]string) {
	selected := configuredSelections(global)
	expressions := selectionExpressions(global)
	for name, selection := range project.Skills {
		if selection.Enabled == nil {
			continue
		}
		if selection.Enabled.Boolean == nil {
			selected[name] = true
			expressions[name] = selection.Enabled.Expression
			continue
		}
		selected[name] = *selection.Enabled.Boolean
		delete(expressions, name)
	}
	return selected, expressions
}

func skillEnabled(selected map[string]bool, skill discoveredSkill) bool {
	enabled, explicit := selected[skill.Name]
	if explicit {
		return enabled
	}
	return skill.Source == projectSkillSource || skill.DisableModelInvocation
}

type enabledEvaluator struct {
	project     string
	callHandler interp.CallHandlerFunc
	evidence    *projectEvidenceIndex
}

func newEnabledEvaluator(project string) *enabledEvaluator {
	evidence := newProjectEvidenceIndex(project)
	return &enabledEvaluator{
		project:     project,
		callHandler: enabledCallHandlerWithEvidence(evidence),
		evidence:    evidence,
	}
}

func (e *enabledEvaluator) evaluate(ctx context.Context, skill, expression string) (bool, error) {
	program, err := syntax.NewParser(
		syntax.Variant(syntax.LangBash),
	).Parse(strings.NewReader(expression), "enabled")
	if err != nil {
		return false, fmt.Errorf("parse enabled expression for skill %q: %w", skill, err)
	}
	runner, err := interp.New(
		interp.Dir(e.project),
		interp.StdIO(nil, io.Discard, io.Discard),
		interp.CallHandler(e.callHandler),
	)
	if err != nil {
		return false, fmt.Errorf("prepare enabled expression for skill %q: %w", skill, err)
	}
	err = runner.Run(ctx, program)
	if err == nil {
		return true, nil
	}
	if status, ok := errors.AsType[interp.ExitStatus](err); ok {
		if status == 1 {
			return false, nil
		}
		return false, fmt.Errorf(
			"evaluate enabled expression for skill %q: status %d",
			skill,
			uint8(status),
		)
	}
	return false, fmt.Errorf("evaluate enabled expression for skill %q: %w", skill, err)
}

func (s selectionState) usesProjectEvidence() bool {
	for name, expression := range s.expressions {
		if !s.selected[name] {
			continue
		}
		program, err := syntax.NewParser(
			syntax.Variant(syntax.LangBash),
		).Parse(strings.NewReader(expression), "enabled")
		if err != nil {
			continue
		}
		found := false
		syntax.Walk(program, func(node syntax.Node) bool {
			call, ok := node.(*syntax.CallExpr)
			if !ok || len(call.Args) == 0 {
				return !found
			}
			switch call.Args[0].Lit() {
			case dependencyBuiltin, languageBuiltin, toolingBuiltin:
				found = true
			}
			return !found
		})
		if found {
			return true
		}
	}
	return false
}
