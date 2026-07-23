package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestSkillListScrollsToKeepCursorVisible(t *testing.T) {
	current := model{
		skills:   skillNames(8),
		selected: map[string]bool{"skill-04": true},
	}
	updated, _ := current.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	current = updated.(model)
	for range 5 {
		updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyDown})
		current = updated.(model)
	}

	if current.cursor != 5 || current.offset != 2 {
		t.Fatalf("cursor, offset = %d, %d; want 5, 2", current.cursor, current.offset)
	}
	view := current.View()
	for _, want := range []string{"  ▸ skill-04", "> ▸ skill-05 [disabled]"} {
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
	updated, _ := current.Update(tea.WindowSizeMsg{Width: 80, Height: 8})
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
	updated, _ := current.Update(tea.WindowSizeMsg{Width: 80, Height: 2})
	current = updated.(model)

	if current.cursor != 2 || current.offset != 2 {
		t.Fatalf("cursor, offset = %d, %d; want 2, 2", current.cursor, current.offset)
	}
	for _, skill := range current.skills {
		if strings.Contains(current.View(), skill.Name) {
			t.Fatalf("tiny window rendered list item %q:\n%s", skill.Name, current.View())
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

func TestSkillListPrimaryClickSelectsAndTogglesHeader(t *testing.T) {
	current := model{
		skills:   skillNames(4),
		selected: map[string]bool{},
		width:    40,
		height:   12,
	}
	updated, command := current.Update(tea.MouseMsg{
		X:      10,
		Y:      tuiHeaderHeight + 2,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	if command != nil {
		t.Fatal("click returned a command")
	}
	current = updated.(model)
	if current.cursor != 2 || current.expanded != "skill-02" {
		t.Fatalf("cursor, expanded = %d, %q; want 2, skill-02", current.cursor, current.expanded)
	}

	updated, _ = current.Update(tea.MouseMsg{
		X:      10,
		Y:      tuiHeaderHeight + 2,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	current = updated.(model)
	if current.cursor != 2 || current.expanded != "" {
		t.Fatalf("second click left cursor, expanded = %d, %q; want 2, empty", current.cursor, current.expanded)
	}
}

func TestSkillListClickAccountsForExpandedRows(t *testing.T) {
	current := model{
		skills:   skillNames(3),
		selected: map[string]bool{},
		expanded: "skill-00",
		width:    40,
		height:   14,
	}
	current.syncViewport()
	secondHeader := tuiHeaderHeight + len(current.skillLines(0))

	updated, _ := current.Update(tea.MouseMsg{
		X:      1,
		Y:      secondHeader,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	got := updated.(model)
	if got.cursor != 1 || got.expanded != "skill-01" {
		t.Fatalf("cursor, expanded = %d, %q; want 1, skill-01", got.cursor, got.expanded)
	}
}

func TestSkillListClickIgnoresDetailsChromeAndOtherButtons(t *testing.T) {
	current := model{
		skills:   skillNames(3),
		selected: map[string]bool{},
		cursor:   1,
		expanded: "skill-01",
		width:    40,
		height:   14,
	}
	current.syncViewport()
	for _, message := range []tea.MouseMsg{
		{X: 1, Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft},
		{X: 1, Y: tuiHeaderHeight + 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft},
		{X: 1, Y: current.height - 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft},
		{X: 1, Y: tuiHeaderHeight, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft},
		{X: 1, Y: tuiHeaderHeight, Action: tea.MouseActionPress, Button: tea.MouseButtonRight},
		{X: current.width, Y: tuiHeaderHeight, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft},
	} {
		updated, command := current.Update(message)
		if command != nil {
			t.Fatalf("ignored mouse message %#v returned a command", message)
		}
		got := updated.(model)
		if got.cursor != current.cursor || got.expanded != current.expanded {
			t.Fatalf(
				"ignored mouse message %#v changed cursor, expanded to %d, %q",
				message,
				got.cursor,
				got.expanded,
			)
		}
	}
}

func TestTUIStylesUseColorWithoutReplacingTextState(t *testing.T) {
	for name, style := range map[string]lipgloss.Style{
		"title":      titleStyle,
		"disclosure": disclosureStyle,
		"disabled":   disabledStyle,
		"muted":      mutedStyle,
		"path":       pathStyle,
		"success":    statusStyle("enabled alpha"),
		"warning":    statusStyle("updating alpha"),
		"error":      statusStyle("error: failed"),
	} {
		if style.GetForeground() == nil {
			t.Errorf("%s style has no foreground color", name)
		}
	}
	if selectedStyle(lipgloss.NewStyle(), true).GetBackground() == nil {
		t.Fatal("selected style has no background color")
	}
	if !pathStyle.GetUnderline() || mutedStyle.GetUnderline() {
		t.Fatal("path value should be underlined independently of its label and indentation")
	}

	current := model{
		skills:   []discoveredSkill{{Name: "alpha", Source: "user"}},
		selected: map[string]bool{},
		width:    30,
		height:   8,
	}
	view := current.View()
	for _, fallback := range []string{"▸", "alpha", "[disabled]", "(user)"} {
		if !strings.Contains(view, fallback) {
			t.Errorf("colored view omitted textual fallback %q:\n%s", fallback, view)
		}
	}
}

func TestStyledPathLinesWrapOnlyPathCharacters(t *testing.T) {
	const width = 20
	lines := styledPathLines("/skills/世界/alpha/SKILL.md", width)
	if len(lines) < 2 {
		t.Fatalf("path did not wrap: %#v", lines)
	}
	for index, line := range lines {
		if lipgloss.Width(line) > width {
			t.Errorf("line %d width = %d, want <= %d: %q", index, lipgloss.Width(line), width, line)
		}
	}
	if !strings.Contains(lines[0], "path  /skills/") {
		t.Fatalf("first path line does not contain label and value: %q", lines[0])
	}
	if strings.Contains(lines[1], "path") {
		t.Fatalf("continuation repeated path label: %q", lines[1])
	}
}

func TestTerminalSafeTextEscapesControlCharacters(t *testing.T) {
	path := "before\x1b\n\u009bafter"
	safe := terminalSafeText(path)
	if safe != `before\x1b\x0a\x9bafter` {
		t.Fatalf("safe path = %q", safe)
	}
	for _, control := range []string{"\x1b", "\n", "\u009b"} {
		if strings.Contains(safe, control) {
			t.Fatalf("safe path retains control character %q: %q", control, safe)
		}
	}

	current := model{
		project: path,
		skills: []discoveredSkill{{
			Name: "alpha", Description: "Alpha.", Path: path, Source: "plugin",
		}},
		selected: map[string]bool{"alpha": true},
		expanded: "alpha",
		width:    40,
		height:   12,
	}
	view := current.View()
	if strings.Contains(view, "\x1b]") || strings.Count(view, `\x1b\x0a\x9b`) != 2 {
		t.Fatalf("view did not safely render filesystem controls: %q", view)
	}
}

func TestEveryRenderedLineFitsNarrowTerminal(t *testing.T) {
	for _, width := range []int{1, 4, 5, 9, 20} {
		current := model{
			project: strings.Repeat("project/", 20),
			skills: []discoveredSkill{{
				Name:        strings.Repeat("n", maxSkillNameLen),
				Description: strings.Repeat("wide 世界 ", 20),
				Path:        "/" + strings.Repeat("long-path/", 20) + "SKILL.md",
				Source:      "plugin",
			}},
			selected: map[string]bool{},
			expanded: strings.Repeat("n", maxSkillNameLen),
			status:   "error: " + strings.Repeat("failed ", 20),
			width:    width,
			height:   30,
		}
		for lineNumber, line := range strings.Split(current.View(), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("width %d line %d has display width %d: %q", width, lineNumber, got, line)
			}
		}
	}
}

func TestEditDoneRefreshesMetadataByCanonicalPath(t *testing.T) {
	const path = "/skills/alpha/SKILL.md"
	current := model{
		skills: []discoveredSkill{{
			Name: "alpha", Description: "Old.", Path: path, Source: "user",
		}},
		selected: map[string]bool{"alpha": true},
		expanded: "alpha",
		busy:     true,
	}
	updated, _ := current.Update(editDone{
		skill: "alpha",
		path:  path,
		skills: []discoveredSkill{{
			Name: "renamed", Description: "New.", Path: path, Source: "user",
		}},
		selected: map[string]bool{"renamed": true},
	})
	got := updated.(model)
	if got.busy || got.skills[0].Name != "renamed" || got.skills[0].Description != "New." {
		t.Fatalf("edit refresh produced model %#v", got)
	}
	if got.cursor != 0 || got.expanded != "renamed" {
		t.Fatalf("cursor, expanded = %d, %q; want 0, renamed", got.cursor, got.expanded)
	}
}

func TestEditDoneRefreshErrorPreservesMetadata(t *testing.T) {
	current := model{
		skills: []discoveredSkill{{
			Name: "alpha", Description: "Old.", Path: "/skills/alpha/SKILL.md",
		}},
		selected: map[string]bool{"alpha": true},
		expanded: "alpha",
		busy:     true,
	}
	updated, _ := current.Update(editDone{
		skill: "alpha", refreshErr: errors.New("rediscovery failed"),
	})
	got := updated.(model)
	if got.busy || got.skills[0].Description != "Old." || got.expanded != "alpha" {
		t.Fatalf("refresh error changed metadata: %#v", got)
	}
	if got.status != "refresh failed: rediscovery failed" {
		t.Fatalf("status = %q", got.status)
	}
}

func TestRefreshEditedSkillMigratesRenamedSelection(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md")
	writeFile(t, path, skillFile("alpha", "Old.", ""))
	if err := saveLock(project, lock{Skills: map[string]bool{"alpha": true}}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, skillFile("renamed", "New.", ""))

	skills, selected, err := manager.refreshEditedSkill(project, "alpha", path)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "renamed" || skills[0].Description != "New." {
		t.Fatalf("skills = %#v", skills)
	}
	if !selected["renamed"] {
		t.Fatalf("renamed skill is not enabled: %#v", selected)
	}
	if _, exists := selected["alpha"]; exists {
		t.Fatalf("obsolete selection was retained: %#v", selected)
	}

	value, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if !value.Skills["renamed"] {
		t.Fatalf("persisted selection = %#v", value.Skills)
	}
	var output strings.Builder
	if err := manager.list(project, &output); err != nil {
		t.Fatalf("list after rename: %v", err)
	}
	if !strings.Contains(output.String(), "## renamed") {
		t.Fatalf("list omitted renamed skill:\n%s", output.String())
	}
}

func TestSkillListIgnoresClicksWhileBusy(t *testing.T) {
	current := model{
		skills:   skillNames(2),
		selected: map[string]bool{},
		width:    40,
		height:   10,
		busy:     true,
	}
	updated, command := current.Update(tea.MouseMsg{
		X:      1,
		Y:      tuiHeaderHeight + 1,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	got := updated.(model)
	if command != nil || got.cursor != 0 || got.expanded != "" {
		t.Fatalf("busy click changed model to %#v with command %v", got, command)
	}
}

func TestSkillListResizeKeepsSelectionVisible(t *testing.T) {
	current := model{
		skills:   skillNames(10),
		selected: map[string]bool{},
		cursor:   7,
		width:    80,
		height:   12,
	}
	current.syncViewport()

	updated, _ := current.Update(tea.WindowSizeMsg{Width: 80, Height: 8})
	current = updated.(model)
	if current.offset != 6 {
		t.Fatalf("offset after resize = %d, want 6", current.offset)
	}
	if !strings.Contains(current.View(), "> ▸ skill-07 [disabled]") {
		t.Fatalf("selected skill is not visible after resize:\n%s", current.View())
	}
}

func TestSkillListShowsSourceAndDisabledState(t *testing.T) {
	current := model{
		skills: []discoveredSkill{
			{Name: "enabled", Source: "project"},
			{Name: "disabled", Source: "user"},
			{Name: "future"},
		},
		selected: map[string]bool{"enabled": true},
		width:    40,
		height:   12,
	}

	view := current.View()
	for _, want := range []string{
		"> ▸ enabled                    (project)",
		"  ▸ disabled [disabled]           (user)",
		"  ▸ future [disabled]          (unknown)",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("view does not contain %q:\n%s", want, view)
		}
	}
	if strings.Contains(strings.Split(view, "\n")[3], "[disabled]") {
		t.Fatalf("enabled skill is labeled disabled:\n%s", view)
	}
}

func TestSkillListExpandsAndCollapsesDetails(t *testing.T) {
	current := model{
		skills: []discoveredSkill{{
			Name:        "alpha",
			Description: "Alpha handles unicode safely: 世界.",
			Path:        "/skills/alpha/SKILL.md",
			Source:      "user",
		}},
		selected: map[string]bool{"alpha": true},
		width:    32,
		height:   14,
	}

	updated, command := current.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil {
		t.Fatal("expanding details returned a command")
	}
	current = updated.(model)
	view := current.View()
	for _, want := range []string{"▾ alpha", "Alpha handles unicode", "safely: 世界.", "path  /skills/alpha/SKILL.md"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expanded view does not contain %q:\n%s", want, view)
		}
	}

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyEnter})
	current = updated.(model)
	view = current.View()
	if current.expanded != "" || strings.Contains(view, "Alpha handles") || strings.Contains(view, "SKILL.md") {
		t.Fatalf("collapsed view retained details:\n%s", view)
	}
}

func TestExpandedSkillParticipatesInViewportScrolling(t *testing.T) {
	current := model{
		skills:   skillNames(5),
		selected: map[string]bool{},
		cursor:   3,
		expanded: "skill-03",
		width:    24,
		height:   11,
	}
	current.syncViewport()

	if current.offset != 3 {
		t.Fatalf("offset = %d, want expanded skill at viewport start", current.offset)
	}
	view := current.View()
	if !strings.Contains(view, "> ▾ skill-03") || strings.Contains(view, "skill-02") {
		t.Fatalf("expanded skill is not visible within viewport:\n%s", view)
	}
}

func skillNames(count int) []discoveredSkill {
	names := make([]discoveredSkill, count)
	for index := range names {
		name := fmt.Sprintf("skill-%02d", index)
		names[index] = discoveredSkill{
			Name:        name,
			Description: "Description for " + name + ".",
			Path:        "/skills/" + name + "/SKILL.md",
			Source:      "user",
		}
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
