package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-shellwords"
	"golang.org/x/term"
)

type model struct {
	manager  *manager
	project  string
	skills   []discoveredSkill
	selected map[string]bool
	cursor   int
	offset   int
	width    int
	height   int
	expanded string
	busy     bool
	status   string
}

const (
	tuiHeaderHeight = 3
	tuiFooterHeight = 3
	tuiChromeHeight = tuiHeaderHeight + tuiFooterHeight
)

var (
	accentColor   = lipgloss.AdaptiveColor{Light: "#005F87", Dark: "#7DD3FC"}
	disabledColor = lipgloss.AdaptiveColor{Light: "#C01C4A", Dark: "#FF7A90"}
	mutedColor    = lipgloss.AdaptiveColor{Light: "#626262", Dark: "#808080"}
	successColor  = lipgloss.AdaptiveColor{Light: "#237A3B", Dark: "#75D18B"}
	warningColor  = lipgloss.AdaptiveColor{Light: "#8A5A00", Dark: "#E5C07B"}
	errorColor    = lipgloss.AdaptiveColor{Light: "#B42318", Dark: "#FF6B6B"}
	selectedColor = lipgloss.AdaptiveColor{Light: "#DCEEFF", Dark: "#30363D"}

	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(accentColor)
	mutedStyle      = lipgloss.NewStyle().Foreground(mutedColor)
	disclosureStyle = lipgloss.NewStyle().Foreground(accentColor)
	disabledStyle   = lipgloss.NewStyle().Bold(true).Foreground(disabledColor)
	pathStyle       = lipgloss.NewStyle().Foreground(accentColor).Underline(true)
)

type toggleDone struct {
	skill   string
	enabled bool
	err     error
}

type editDone struct {
	skill      string
	path       string
	skills     []discoveredSkill
	selected   map[string]bool
	editorErr  error
	refreshErr error
}

func newModel(manager *manager, project string) (model, error) {
	discovered, err := manager.skills(project)
	if err != nil {
		return model{}, err
	}
	selected, err := manager.selection(project)
	if err != nil {
		return model{}, err
	}
	return model{
		manager: manager, project: project, skills: discovered, selected: selected,
		status: fmt.Sprintf("%d skills", len(discovered)),
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
		if message.editorErr != nil {
			m.status = "editor failed: " + message.editorErr.Error()
			break
		}
		if message.refreshErr != nil {
			m.status = "refresh failed: " + message.refreshErr.Error()
			break
		}
		wasExpanded := m.expanded == message.skill
		m.skills = message.skills
		m.selected = message.selected
		m.expanded = ""
		for index, skill := range m.skills {
			if skill.Path != message.path {
				continue
			}
			m.cursor = index
			if wasExpanded {
				m.expanded = skill.Name
			}
			break
		}
		m.syncViewport()
		m.status = "saved " + message.skill
	case tea.WindowSizeMsg:
		m.width = message.Width
		m.height = message.Height
		m.syncViewport()
	case tea.MouseMsg:
		if m.busy {
			return m, nil
		}
		mouse := tea.MouseEvent(message)
		if mouse.Action != tea.MouseActionPress || mouse.Button != tea.MouseButtonLeft {
			return m, nil
		}
		if mouse.X < 0 || mouse.X >= m.width {
			return m, nil
		}
		index, ok := m.skillIndexAtHeaderRow(mouse.Y)
		if !ok {
			return m, nil
		}
		m.toggleExpanded(index)
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
		case "enter":
			m.toggleExpanded(m.cursor)
		case " ":
			if m.cursor < 0 || m.cursor >= len(m.skills) {
				break
			}
			skill := m.skills[m.cursor].Name
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
			command, err := m.editor(skill.Name)
			if err != nil {
				m.status = "error: " + err.Error()
				break
			}
			m.busy = true
			m.status = "editing " + skill.Name
			return m, tea.ExecProcess(command, func(err error) tea.Msg {
				if err != nil {
					return editDone{skill: skill.Name, editorErr: err}
				}
				skills, selected, refreshErr := m.manager.refreshEditedSkill(
					m.project,
					skill.Name,
					skill.Path,
				)
				return editDone{
					skill: skill.Name, path: skill.Path,
					skills: skills, selected: selected, refreshErr: refreshErr,
				}
			})
		}
	}
	return m, nil
}

func (m *manager) refreshEditedSkill(
	project string,
	oldName string,
	path string,
) ([]discoveredSkill, map[string]bool, error) {
	skills, err := m.skills(project)
	if err != nil {
		return nil, nil, err
	}
	value, err := loadLock(project)
	if err != nil {
		return nil, nil, err
	}
	newName := ""
	for _, skill := range skills {
		if skill.Path == path {
			newName = skill.Name
			break
		}
	}
	if newName == "" || newName == oldName {
		return skills, value.Skills, nil
	}
	oldEnabled, exists := value.Skills[oldName]
	if !exists {
		return skills, value.Skills, nil
	}
	value.Skills[newName] = value.Skills[newName] || oldEnabled
	delete(value.Skills, oldName)
	if err := saveLock(project, value); err != nil {
		return nil, nil, err
	}
	return skills, value.Skills, nil
}

func (m *model) toggleExpanded(index int) {
	if index < 0 || index >= len(m.skills) {
		return
	}
	m.cursor = index
	skill := m.skills[index].Name
	if m.expanded == skill {
		m.expanded = ""
	} else {
		m.expanded = skill
	}
	m.syncViewport()
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
	if m.offset < 0 {
		m.offset = 0
	} else if m.offset >= len(m.skills) {
		m.offset = len(m.skills) - 1
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	for m.offset < m.cursor && m.rowCount(m.offset, m.cursor+1) > visible {
		m.offset++
	}
	for m.offset > 0 && m.rowCount(m.offset-1, m.cursor+1) <= visible {
		m.offset--
	}
}

func (m model) View() string {
	m.syncViewport()
	var view strings.Builder
	fmt.Fprintf(
		&view,
		"%s\n%s\n\n",
		boundLine(titleStyle.Render("Skill Manager"), m.width),
		boundLine(mutedStyle.Render(terminalSafeText(m.project)), m.width),
	)
	remaining := m.height - tuiChromeHeight
	for index := m.offset; index < len(m.skills) && remaining > 0; index++ {
		lines := m.skillLines(index)
		if len(lines) > remaining {
			lines = lines[:remaining]
		}
		for _, line := range lines {
			fmt.Fprintln(&view, line)
		}
		remaining -= len(lines)
	}
	fmt.Fprintf(
		&view,
		"\n%s\n%s\n",
		boundLine(
			mutedStyle.Render("↑/k ↓/j move • enter/click details • space toggle • e edit • q quit"),
			m.width,
		),
		boundLine(statusStyle(m.status).Render(terminalSafeText(m.status)), m.width),
	)
	return view.String()
}

func boundLine(line string, width int) string {
	if width <= 0 {
		return ""
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(line)
}

func statusStyle(status string) lipgloss.Style {
	switch {
	case strings.HasPrefix(status, "error:"),
		strings.HasPrefix(status, "editor failed:"),
		strings.HasPrefix(status, "refresh failed:"):
		return lipgloss.NewStyle().Foreground(errorColor)
	case strings.HasPrefix(status, "updating "), strings.HasPrefix(status, "editing "):
		return lipgloss.NewStyle().Foreground(warningColor)
	case strings.HasPrefix(status, "enabled "), strings.HasPrefix(status, "saved "):
		return lipgloss.NewStyle().Foreground(successColor)
	case strings.HasPrefix(status, "disabled "):
		return lipgloss.NewStyle().Foreground(disabledColor)
	default:
		return lipgloss.NewStyle().Foreground(mutedColor)
	}
}

func (m model) skillIndexAtHeaderRow(y int) (int, bool) {
	if y < tuiHeaderHeight || y >= m.height-tuiFooterHeight {
		return 0, false
	}
	row := tuiHeaderHeight
	remaining := m.height - tuiChromeHeight
	for index := m.offset; index < len(m.skills) && remaining > 0; index++ {
		if y == row {
			return index, true
		}
		rendered := min(m.skillLineCount(index), remaining)
		row += rendered
		remaining -= rendered
	}
	return 0, false
}

func (m model) rowCount(start, end int) int {
	rows := 0
	for index := start; index < end; index++ {
		rows += m.skillLineCount(index)
	}
	return rows
}

func (m model) skillLineCount(index int) int {
	skill := m.skills[index]
	if m.expanded != skill.Name {
		return 1
	}
	return 1 +
		len(wrapDetail(skill.Description, m.width)) +
		pathLineCount(terminalSafeText(skill.Path), m.width)
}

func (m model) skillLines(index int) []string {
	skill := m.skills[index]
	cursor, disclosure := " ", "▸"
	if index == m.cursor {
		cursor = ">"
	}
	if m.expanded == skill.Name {
		disclosure = "▾"
	}
	prefix := fmt.Sprintf("%s %s %s", cursor, disclosure, skill.Name)
	if !m.selected[skill.Name] {
		prefix += " [disabled]"
	}
	source := skill.Source
	if source == "" {
		source = "unknown"
	}
	label := "(" + source + ")"
	gap := max(m.width-lipgloss.Width(prefix)-lipgloss.Width(label), 1)
	selected := index == m.cursor
	name := selectedStyle(lipgloss.NewStyle(), selected).Render(cursor + " ")
	name += selectedStyle(disclosureStyle, selected).Render(disclosure + " ")
	name += selectedStyle(lipgloss.NewStyle(), selected).Render(skill.Name)
	if !m.selected[skill.Name] {
		name += selectedStyle(disabledStyle, selected).Render(" [disabled]")
	}
	line := name
	line += selectedStyle(lipgloss.NewStyle(), selected).Render(strings.Repeat(" ", gap))
	line += selectedStyle(mutedStyle, selected).Render(label)
	lines := []string{boundLine(line, m.width)}
	if m.expanded != skill.Name {
		return lines
	}
	for _, detail := range wrapDetail(skill.Description, m.width) {
		lines = append(lines, boundLine(mutedStyle.Render(detail), m.width))
	}
	lines = append(lines, styledPathLines(terminalSafeText(skill.Path), m.width)...)
	return lines
}

func selectedStyle(style lipgloss.Style, selected bool) lipgloss.Style {
	if !selected {
		return style
	}
	return style.Background(selectedColor).Bold(true)
}

func wrapDetail(value string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	indent := strings.Repeat(" ", min(4, max(0, width-2)))
	available := width - lipgloss.Width(indent)
	var lines []string
	for lipgloss.Width(value) > available {
		runes := []rune(value)
		cut := displayWidthCut(runes, available)
		head := string(runes[:cut])
		tail := string(runes[cut:])
		if space := strings.LastIndexByte(head, ' '); space > 0 {
			tail = head[space:] + tail
			head = head[:space]
		}
		lines = append(lines, indent+strings.TrimSpace(head))
		value = strings.TrimSpace(tail)
	}
	return append(lines, indent+value)
}

func styledPathLines(path string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	prefix := pathPrefix(width)
	available := width - lipgloss.Width(prefix)
	chunks := hardWrap(path, available)
	lines := make([]string, len(chunks))
	for index, chunk := range chunks {
		padding := prefix
		if index > 0 {
			padding = strings.Repeat(" ", lipgloss.Width(prefix))
		}
		lines[index] = boundLine(mutedStyle.Render(padding)+pathStyle.Render(chunk), width)
	}
	return lines
}

func pathLineCount(path string, width int) int {
	if width <= 0 {
		return 1
	}
	return len(hardWrap(path, width-lipgloss.Width(pathPrefix(width))))
}

func pathPrefix(width int) string {
	const label = "path  "
	if width <= lipgloss.Width(label) {
		return ""
	}
	indent := strings.Repeat(" ", min(4, width-lipgloss.Width(label)-1))
	return indent + label
}

func hardWrap(value string, width int) []string {
	if value == "" || width <= 0 {
		return []string{value}
	}
	var lines []string
	for lipgloss.Width(value) > width {
		runes := []rune(value)
		cut := displayWidthCut(runes, width)
		lines = append(lines, string(runes[:cut]))
		value = string(runes[cut:])
	}
	return append(lines, value)
}

func displayWidthCut(runes []rune, width int) int {
	cut := 0
	for cut < len(runes) && lipgloss.Width(string(runes[:cut+1])) <= width {
		cut++
	}
	if cut == 0 {
		return 1
	}
	return cut
}

func terminalSafeText(value string) string {
	var safe strings.Builder
	for _, character := range value {
		if !unicode.IsControl(character) {
			safe.WriteRune(character)
			continue
		}
		if character <= 0xff {
			fmt.Fprintf(&safe, "\\x%02x", character)
		} else {
			fmt.Fprintf(&safe, "\\u%04x", character)
		}
	}
	return safe.String()
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
	_, err = tea.NewProgram(current, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	return err
}
