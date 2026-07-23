package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEditorTargetsCanonicalSkill(t *testing.T) {
	manager := newTestManager(t)
	skill := filepath.Join(manager.paths.library, "alpha", "SKILL.md")
	writeFile(t, skill, "before")
	editor := filepath.Join(t.TempDir(), "editor")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf edited > \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", editor+" --flag")

	command, err := (model{manager: manager}).editor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	assertFile(t, skill, "edited")
}
