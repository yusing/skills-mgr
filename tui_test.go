package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSkillListScrollsToKeepCursorVisible(t *testing.T) {
	current := model{
		skills:   skillNames(8),
		selected: map[string]bool{"skill-04": true},
	}
	updated, _ := current.Update(tea.WindowSizeMsg{Height: 10})
	current = updated.(model)
	for range 5 {
		updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyDown})
		current = updated.(model)
	}

	if current.cursor != 5 || current.offset != 2 {
		t.Fatalf("cursor, offset = %d, %d; want 5, 2", current.cursor, current.offset)
	}
	view := current.View()
	for _, want := range []string{"  [x] skill-04\n", "> [ ] skill-05\n"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view does not contain %q:\n%s", want, view)
		}
	}
	for _, hidden := range []string{"skill-00", "skill-01", "skill-06"} {
		if strings.Contains(view, hidden) {
			t.Fatalf("view contains skill outside viewport %q:\n%s", hidden, view)
		}
	}
}

func TestSkillListStopsAtNavigationBoundaries(t *testing.T) {
	current := model{skills: skillNames(3), selected: map[string]bool{}}
	updated, _ := current.Update(tea.WindowSizeMsg{Height: 8})
	current = updated.(model)

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyUp})
	current = updated.(model)
	if current.cursor != 0 || current.offset != 0 {
		t.Fatalf("up at start moved cursor, offset to %d, %d", current.cursor, current.offset)
	}

	for range 4 {
		updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyDown})
		current = updated.(model)
	}
	if current.cursor != 2 || current.offset != 1 {
		t.Fatalf("down past end moved cursor, offset to %d, %d; want 2, 1", current.cursor, current.offset)
	}
}

func TestSkillListHandlesTinyWindowAndMalformedViewportState(t *testing.T) {
	current := model{
		skills:   skillNames(3),
		selected: map[string]bool{},
		cursor:   99,
		offset:   -4,
	}
	updated, _ := current.Update(tea.WindowSizeMsg{Height: 2})
	current = updated.(model)

	if current.cursor != 2 || current.offset != 2 {
		t.Fatalf("cursor, offset = %d, %d; want 2, 2", current.cursor, current.offset)
	}
	for _, skill := range current.skills {
		if strings.Contains(current.View(), skill) {
			t.Fatalf("tiny window rendered list item %q:\n%s", skill, current.View())
		}
	}
}

func TestSkillListIgnoresUnknownInputAndFutureMessages(t *testing.T) {
	current := model{
		skills:   skillNames(5),
		selected: map[string]bool{},
		cursor:   2,
		offset:   1,
		height:   9,
	}
	for _, message := range []tea.Msg{
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")},
		struct{ future string }{future: "message"},
	} {
		updated, command := current.Update(message)
		if command != nil {
			t.Fatalf("unknown message %#v returned a command", message)
		}
		got := updated.(model)
		if got.cursor != current.cursor || got.offset != current.offset {
			t.Fatalf("unknown message %#v changed cursor, offset to %d, %d", message, got.cursor, got.offset)
		}
	}
}

func TestSkillListResizeKeepsSelectionVisible(t *testing.T) {
	current := model{
		skills:   skillNames(10),
		selected: map[string]bool{},
		cursor:   7,
		height:   12,
	}
	current.syncViewport()

	updated, _ := current.Update(tea.WindowSizeMsg{Height: 8})
	current = updated.(model)
	if current.offset != 6 {
		t.Fatalf("offset after resize = %d, want 6", current.offset)
	}
	if !strings.Contains(current.View(), "> [ ] skill-07\n") {
		t.Fatalf("selected skill is not visible after resize:\n%s", current.View())
	}
}

func skillNames(count int) []string {
	names := make([]string, count)
	for index := range names {
		names[index] = fmt.Sprintf("skill-%02d", index)
	}
	return names
}

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
