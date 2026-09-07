package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func aliasTestDirectory(t *testing.T, target string) string {
	t.Helper()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	return alias
}

func TestRelocateSkillThroughAncestorAlias(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	home := aliasTestDirectory(t, manager.paths.placeholderDir)
	manager.paths.userSkills = filepath.Join(home, ".agents", "skills")
	manager.paths.managedSkills = filepath.Join(home, ".skills-mgr", "skills")
	content := skillFile("drafting", "Draft specs.", "body\n")
	source := filepath.Join(manager.paths.userSkills, "drafting", "SKILL.md")
	destination := filepath.Join(manager.paths.managedSkills, "drafting", "SKILL.md")
	writeFile(t, source, content)
	for _, want := range []string{destination, source} {
		skill, err := manager.findSkill(project, "drafting")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.relocateSkill(project, skill); err != nil {
			t.Fatal(err)
		}
		assertFile(t, want, content)
	}
}

func TestRefreshEditedSkillThroughAncestorAlias(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(manager.paths.managedSkills, "alpha", "SKILL.md")
	writeFile(t, path, skillFile("alpha", "Old.", ""))
	alias := aliasTestDirectory(t, manager.paths.managerHome)
	editedPath := filepath.Join(alias, "skills", "alpha", "SKILL.md")
	if err := saveLock(project, testLock(map[string]bool{"alpha": true}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	writeFile(t, editedPath, skillFile("renamed", "New.", ""))
	_, selection, err := manager.refreshEditedSkill(project, "alpha", editedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.selected["renamed"] || selection.selected["alpha"] {
		t.Fatalf("selection = %#v", selection.selected)
	}
	value, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if !hasBooleanSelection(value, "renamed", true) {
		t.Fatalf("persisted selection = %#v", value)
	}
	assertFile(t, filepath.Join(project, ".agents", "skills", "renamed", "SKILL.md"), wantPlaceholder("renamed", "New."))
}

func TestRelocateSkillRejectsSymlinkedSourceCollection(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	external := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(manager.paths.userSkills), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, manager.paths.userSkills); err != nil {
		t.Fatal(err)
	}
	content := skillFile("drafting", "Draft specs.", "body\n")
	source := filepath.Join(manager.paths.userSkills, "drafting", "SKILL.md")
	writeFile(t, source, content)
	if err := saveLock(project, testLock(map[string]bool{"drafting": true}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	conflict := filepath.Join(project, ".claude", "skills", "drafting", "SKILL.md")
	conflictContent := skillFile("drafting", "Project conflict.", "body\n")
	writeFile(t, conflict, conflictContent)
	skill, err := manager.findSkill(project, "drafting")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.relocateSkill(project, skill); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("symlinked source collection error = %v", err)
	}
	assertFile(t, source, content)
	assertFile(t, conflict, conflictContent)
	if _, err := os.Stat(filepath.Join(manager.paths.managedSkills, "drafting")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected relocation left managed content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".agents", "skills", "drafting")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected relocation left project placeholder: %v", err)
	}
}
