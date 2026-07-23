package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-shellwords"
	"golang.org/x/term"
)

type model struct {
	manager  *manager
	project  string
	skills   []string
	selected map[string]bool
	cursor   int
	busy     bool
	status   string
}

type toggleDone struct {
	skill   string
	enabled bool
	err     error
}

type editDone struct {
	skill string
	err   error
}

func newModel(manager *manager, project string) (model, error) {
	discovered, err := manager.skills(project)
	if err != nil {
		return model{}, err
	}
	skills := make([]string, 0, len(discovered))
	for _, skill := range discovered {
		skills = append(skills, skill.Name)
	}
	selected, err := manager.selection(project)
	if err != nil {
		return model{}, err
	}
	return model{
		manager: manager, project: project, skills: skills, selected: selected,
		status: fmt.Sprintf("%d skills", len(skills)),
	}, nil
}

func (model) Init() tea.Cmd { return nil }

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch message := message.(type) {
	case toggleDone:
		m.busy = false
		if message.err != nil {
			m.status = "error: " + message.err.Error()
		} else {
			m.selected[message.skill] = message.enabled
			if message.enabled {
				m.status = "enabled " + message.skill
			} else {
				m.status = "disabled " + message.skill
			}
		}
	case editDone:
		m.busy = false
		if message.err != nil {
			m.status = "editor failed: " + message.err.Error()
		} else {
			m.status = "saved " + message.skill
		}
	case tea.KeyMsg:
		if m.busy {
			if message.String() == "ctrl+c" {
				return m, tea.Quit
			}
			return m, nil
		}
		switch message.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor+1 < len(m.skills) {
				m.cursor++
			}
		case " ", "enter":
			if len(m.skills) == 0 {
				break
			}
			skill := m.skills[m.cursor]
			m.busy = true
			m.status = "updating " + skill
			return m, func() tea.Msg {
				enabled, err := m.manager.toggle(m.project, skill)
				return toggleDone{skill: skill, enabled: enabled, err: err}
			}
		case "e":
			if len(m.skills) == 0 {
				break
			}
			skill := m.skills[m.cursor]
			command, err := m.editor(skill)
			if err != nil {
				m.status = "error: " + err.Error()
				break
			}
			m.busy = true
			m.status = "editing " + skill
			return m, tea.ExecProcess(command, func(err error) tea.Msg {
				return editDone{skill: skill, err: err}
			})
		}
	}
	return m, nil
}

func (m model) View() string {
	var view strings.Builder
	fmt.Fprintf(&view, "Skill Manager\n%s\n\n", m.project)
	for index, skill := range m.skills {
		cursor, mark := " ", " "
		if index == m.cursor {
			cursor = ">"
		}
		if m.selected[skill] {
			mark = "x"
		}
		fmt.Fprintf(&view, "%s [%s] %s\n", cursor, mark, skill)
	}
	fmt.Fprintf(&view, "\n↑/k ↓/j move • space toggle • e edit • q quit\n%s\n", m.status)
	return view.String()
}

func (m model) editor(skill string) (*exec.Cmd, error) {
	parts, err := shellwords.Parse(os.Getenv("EDITOR"))
	if err != nil || len(parts) == 0 {
		return nil, fmt.Errorf("$EDITOR is not set or invalid")
	}
	discovered, err := m.manager.findSkill(m.project, skill)
	if err != nil {
		return nil, err
	}
	if !discovered.Editable {
		return nil, fmt.Errorf("skill %q is not editable at its discovered source", skill)
	}
	file := discovered.Path
	if info, err := os.Stat(file); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("missing %s", file)
	}
	return exec.Command(parts[0], append(parts[1:], file)...), nil
}

func runTUI(manager *manager, project string) error {
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("TUI requires a terminal")
	}
	current, err := newModel(manager, project)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(current, tea.WithAltScreen()).Run()
	return err
}
