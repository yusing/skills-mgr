package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditorTargetsCanonicalSkill(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	skill := filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md")
	writeFile(t, skill, skillFile("alpha", "Alpha.", "before"))
	editor := filepath.Join(t.TempDir(), "editor")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf edited > \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", editor+" --flag")

	command, err := (model{manager: manager, project: project}).editor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	assertFile(t, skill, "edited")
}

func TestToggleErrorPreservesSelection(t *testing.T) {
	current := model{
		selected: map[string]bool{"alpha": true},
		busy:     true,
	}

	updated, _ := current.Update(toggleDone{
		skill:   "alpha",
		enabled: false,
		err:     errors.New("write failed"),
	})
	got := updated.(model)
	if !got.selected["alpha"] {
		t.Fatal("toggle error changed the displayed selection")
	}
	if got.busy {
		t.Fatal("toggle error left the model busy")
	}
	if got.status != "error: write failed" {
		t.Fatalf("status = %q", got.status)
	}
}

func TestEditorRejectsNonEditableSkillSource(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeSkill(t, filepath.Join(manager.paths.adminSkills, "admin"), "admin")
	t.Setenv("EDITOR", "editor")

	_, err := (model{manager: manager, project: project}).editor("admin")
	if err == nil || !strings.Contains(err.Error(), "not editable") {
		t.Fatalf("error = %v, want non-editable source error", err)
	}
}
