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
	offset   int
	height   int
	busy     bool
	status   string
}

const tuiChromeHeight = 6

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
	case tea.WindowSizeMsg:
		m.height = message.Height
		m.syncViewport()
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
			m.syncViewport()
		case "down", "j":
			if m.cursor+1 < len(m.skills) {
				m.cursor++
			}
			m.syncViewport()
		case " ", "enter":
			if m.cursor < 0 || m.cursor >= len(m.skills) {
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
			if m.cursor < 0 || m.cursor >= len(m.skills) {
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

func (m *model) syncViewport() {
	if len(m.skills) == 0 {
		m.cursor = 0
		m.offset = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	} else if m.cursor >= len(m.skills) {
		m.cursor = len(m.skills) - 1
	}

	visible := m.height - tuiChromeHeight
	if visible <= 0 {
		m.offset = m.cursor
		return
	}
	maxOffset := len(m.skills) - visible
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.offset < 0 {
		m.offset = 0
	} else if m.offset > maxOffset {
		m.offset = maxOffset
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	} else if m.cursor >= m.offset+visible {
		m.offset = m.cursor - visible + 1
	}
}

func (m model) View() string {
	m.syncViewport()
	var view strings.Builder
	fmt.Fprintf(&view, "Skill Manager\n%s\n\n", m.project)
	visible := m.height - tuiChromeHeight
	end := m.offset + visible
	if visible < 0 {
		end = m.offset
	} else if end > len(m.skills) {
		end = len(m.skills)
	}
	for index := m.offset; index < end; index++ {
		skill := m.skills[index]
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
