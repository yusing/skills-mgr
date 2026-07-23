package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-shellwords"
	"golang.org/x/term"
)

type model struct {
	manager         *manager
	project         string
	skills          []discoveredSkill
	selected        map[string]bool
	remoteTopics    []remoteTopic
	remoteCollapsed map[string]bool
	remoteError     string
	skillsMPSkills  []skillsMPSkill
	skillsMPError   string
	skillsMPCancel  context.CancelFunc
	skillsMPRequest uint64
	tab             int
	cursor          int
	offset          int
	width           int
	height          int
	expanded        string
	busy            bool
	status          string
	filtering       bool
	filterQuery     string
}

const (
	tuiHeaderHeight  = 5
	tuiFooterHeight  = 2
	tuiChromeHeight  = tuiHeaderHeight + tuiFooterHeight
	localTab         = 0
	remoteTab        = 1
	skillsMPTab      = 2
	skillsMPDebounce = 300 * time.Millisecond
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

type skillsMPSearchDone struct {
	request uint64
	query   string
	skills  []skillsMPSkill
	err     error
}

type skillsMPSearchRequested struct {
	request uint64
	query   string
}

type skillsMPCatalogDone struct {
	request uint64
	skills  []skillsMPSkill
	err     error
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
	current := model{
		manager: manager, project: project, skills: discovered, selected: selected,
		remoteCollapsed: make(map[string]bool),
		status:          fmt.Sprintf("%d skills", len(discovered)),
	}
	current.reloadRemoteCache()
	return current, nil
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
		for index, skillIndex := range m.localSkillIndices() {
			skill := m.skills[skillIndex]
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
	case skillsMPSearchDone:
		if message.request != m.skillsMPRequest ||
			message.query != normalizedSkillsMPQuery(m.filterQuery) ||
			m.tab != skillsMPTab {
			break
		}
		m.skillsMPCancel = nil
		if message.err != nil {
			m.skillsMPSkills = message.skills
			m.skillsMPError = message.err.Error()
			if len(message.skills) == 0 {
				m.status = "error: " + message.err.Error()
			} else {
				m.status = "warning: " + message.err.Error()
			}
			break
		}
		m.skillsMPSkills = message.skills
		m.skillsMPError = ""
		m.status = fmt.Sprintf("%d SkillsMP skills", len(message.skills))
		m.syncViewport()
	case skillsMPSearchRequested:
		if message.request != m.skillsMPRequest ||
			message.query != normalizedSkillsMPQuery(m.filterQuery) ||
			m.tab != skillsMPTab {
			break
		}
		return m, m.startSkillsMPSearch(message.query)
	case skillsMPCatalogDone:
		if message.request != m.skillsMPRequest ||
			m.tab != skillsMPTab ||
			normalizedSkillsMPQuery(m.filterQuery) != "" {
			break
		}
		m.skillsMPCancel = nil
		m.skillsMPSkills = message.skills
		if message.err != nil {
			m.skillsMPError = message.err.Error()
			if len(message.skills) == 0 {
				m.status = "error: " + message.err.Error()
			} else {
				m.status = "warning: " + message.err.Error()
			}
			break
		}
		m.skillsMPError = ""
		m.status = fmt.Sprintf("%d SkillsMP skills", len(message.skills))
		m.syncViewport()
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
		if mouse.Y == tuiHeaderHeight-1 {
			m.startFiltering()
			return m, nil
		}
		if m.filtering {
			return m, nil
		}
		if m.tab != localTab {
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
		if m.filtering {
			switch message.String() {
			case "ctrl+c":
				m.cancelSkillsMPRequest()
				return m, tea.Quit
			case "enter", "esc":
				m.filtering = false
			case "backspace":
				if runes := []rune(m.filterQuery); len(runes) > 0 {
					m.filterQuery = string(runes[:len(runes)-1])
					return m, m.filterChanged()
				}
			default:
				if message.Type == tea.KeyRunes {
					m.filterQuery += string(message.Runes)
					return m, m.filterChanged()
				}
			}
			return m, nil
		}
		switch message.String() {
		case "q", "ctrl+c":
			m.cancelSkillsMPRequest()
			return m, tea.Quit
		case "f":
			m.startFiltering()
		case "left":
			return m, m.selectTab(max(localTab, m.tab-1))
		case "right":
			return m, m.selectTab(min(skillsMPTab, m.tab+1))
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			m.syncViewport()
		case "down", "j":
			if m.cursor+1 < m.itemCount() {
				m.cursor++
			}
			m.syncViewport()
		case "enter":
			if m.tab == remoteTab {
				m.toggleRemoteTopic()
			} else {
				m.toggleExpanded(m.cursor)
			}
		case " ":
			if m.tab != localTab {
				break
			}
			skillIndex, ok := m.localSkillIndex(m.cursor)
			if !ok {
				break
			}
			skill := m.skills[skillIndex].Name
			m.busy = true
			m.status = "updating " + skill
			return m, func() tea.Msg {
				enabled, err := m.manager.toggle(m.project, skill)
				return toggleDone{skill: skill, enabled: enabled, err: err}
			}
		case "e":
			if m.tab != localTab {
				break
			}
			skillIndex, ok := m.localSkillIndex(m.cursor)
			if !ok {
				break
			}
			skill := m.skills[skillIndex]
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

func (m *model) selectTab(tab int) tea.Cmd {
	if tab < localTab || tab > skillsMPTab || m.tab == tab {
		return nil
	}
	m.cancelSkillsMPRequest()
	m.tab = tab
	m.cursor = 0
	m.offset = 0
	switch tab {
	case remoteTab:
		if m.manager != nil {
			m.reloadRemoteCache()
		}
		count := 0
		for _, topic := range m.remoteTopics {
			count += len(topic.Skills)
		}
		if m.remoteError != "" {
			m.status = "error: " + m.remoteError
		} else if len(m.remoteTopics) == 0 {
			m.status = "remote cache is empty; run skills-mgr daemon"
		} else {
			m.status = fmt.Sprintf("%d remote skills in %d topics", count, len(m.remoteTopics))
		}
	case skillsMPTab:
		if normalizedSkillsMPQuery(m.filterQuery) != "" {
			m.status = "searching SkillsMP"
			m.syncViewport()
			return m.debounceSkillsMPSearch()
		}
		cache := m.reloadSkillsMPCache()
		if m.skillsMPError == "" && len(cache.Skills) > 0 &&
			time.Now().Before(cache.UpdatedAt.Add(skillsMPCacheTTL)) {
			m.status = fmt.Sprintf("%d SkillsMP skills", len(m.skillsMPSkills))
			break
		}
		m.status = "loading SkillsMP"
		m.syncViewport()
		return m.startSkillsMPCatalog()
	default:
		m.status = fmt.Sprintf("%d skills", len(m.skills))
	}
	m.syncViewport()
	return nil
}

func (m *model) startFiltering() {
	m.filtering = true
}

func (m *model) filterChanged() tea.Cmd {
	m.cancelSkillsMPRequest()
	m.cursor = 0
	m.offset = 0
	m.expanded = ""
	if m.tab == skillsMPTab {
		if normalizedSkillsMPQuery(m.filterQuery) == "" {
			cache := m.reloadSkillsMPCache()
			if m.skillsMPError == "" && len(cache.Skills) > 0 &&
				time.Now().Before(cache.UpdatedAt.Add(skillsMPCacheTTL)) {
				m.status = fmt.Sprintf("%d SkillsMP skills", len(m.skillsMPSkills))
				return nil
			}
			m.status = "loading SkillsMP"
			return m.startSkillsMPCatalog()
		}
		m.skillsMPSkills = nil
		m.skillsMPError = ""
		m.status = "searching SkillsMP"
		return m.debounceSkillsMPSearch()
	}
	m.syncViewport()
	return nil
}

func (m *model) reloadRemoteCache() {
	cache, err := loadRemoteCache(m.manager.paths.remoteRegistry)
	if err != nil {
		m.remoteTopics = nil
		m.remoteError = err.Error()
		return
	}
	m.remoteTopics = cache.Topics
	m.remoteError = ""
}

func (m *model) reloadSkillsMPCache() skillsMPCache {
	if m.manager == nil {
		return skillsMPCache{}
	}
	cache, err := loadSkillsMPCache(m.manager.paths.skillsMP)
	if err != nil {
		m.skillsMPSkills = nil
		m.skillsMPError = err.Error()
		return skillsMPCache{}
	}
	m.skillsMPSkills = cache.Skills
	m.skillsMPError = ""
	return cache
}

func (m *model) startSkillsMPSearch(query string) tea.Cmd {
	m.cancelSkillsMPRequest()
	request := m.skillsMPRequest
	ctx, cancel := context.WithCancel(context.Background())
	m.skillsMPCancel = cancel
	return func() tea.Msg {
		defer cancel()
		if m.manager == nil || m.manager.skillsMP == nil {
			return skillsMPSearchDone{
				request: request, query: query,
				err: fmt.Errorf("SkillsMP provider is unavailable"),
			}
		}
		skills, err := m.manager.skillsMP.search(ctx, query)
		return skillsMPSearchDone{
			request: request, query: query, skills: skills, err: err,
		}
	}
}

func (m *model) startSkillsMPCatalog() tea.Cmd {
	m.cancelSkillsMPRequest()
	request := m.skillsMPRequest
	ctx, cancel := context.WithCancel(context.Background())
	m.skillsMPCancel = cancel
	return func() tea.Msg {
		defer cancel()
		if m.manager == nil || m.manager.skillsMP == nil {
			return skillsMPCatalogDone{
				request: request, err: fmt.Errorf("SkillsMP provider is unavailable"),
			}
		}
		skills, err := m.manager.skillsMP.catalog(ctx)
		return skillsMPCatalogDone{request: request, skills: skills, err: err}
	}
}

func (m *model) cancelSkillsMPRequest() {
	m.skillsMPRequest++
	if m.skillsMPCancel != nil {
		m.skillsMPCancel()
		m.skillsMPCancel = nil
	}
}

func (m model) debounceSkillsMPSearch() tea.Cmd {
	request := m.skillsMPRequest
	query := normalizedSkillsMPQuery(m.filterQuery)
	return tea.Tick(skillsMPDebounce, func(time.Time) tea.Msg {
		return skillsMPSearchRequested{request: request, query: query}
	})
}

func (m model) itemCount() int {
	if m.tab == remoteTab {
		return len(m.remoteRows())
	}
	if m.tab == skillsMPTab {
		return len(m.skillsMPSkills)
	}
	return len(m.localSkillIndices())
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
	skillIndex, ok := m.localSkillIndex(index)
	if !ok {
		return
	}
	m.cursor = index
	skill := m.skills[skillIndex].Name
	if m.expanded == skill {
		m.expanded = ""
	} else {
		m.expanded = skill
	}
	m.syncViewport()
}

func (m *model) syncViewport() {
	if m.tab == remoteTab {
		m.syncFixedRowViewport(len(m.remoteRows()))
		return
	}
	if m.tab == skillsMPTab {
		m.syncFixedRowViewport(len(m.skillsMPSkills))
		return
	}
	indices := m.localSkillIndices()
	count := len(indices)
	if count == 0 {
		m.cursor = 0
		m.offset = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	} else if m.cursor >= count {
		m.cursor = count - 1
	}

	visible := m.height - tuiChromeHeight
	if visible <= 0 {
		m.offset = m.cursor
		return
	}
	if m.offset < 0 {
		m.offset = 0
	} else if m.offset >= count {
		m.offset = count - 1
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	for m.offset < m.cursor && m.rowCount(indices, m.offset, m.cursor+1) > visible {
		m.offset++
	}
	for m.offset > 0 && m.rowCount(indices, m.offset-1, m.cursor+1) <= visible {
		m.offset--
	}
}

func (m model) View() string {
	m.syncViewport()
	var view strings.Builder
	fmt.Fprintf(
		&view,
		"\n%s\n%s\n%s\n",
		boundLine(titleStyle.Render("Skill Manager"), m.width),
		boundLine(mutedStyle.Render(terminalSafeText(m.project)), m.width),
		m.tabBar(),
	)
	fmt.Fprintln(&view, m.filterLine())
	remaining := m.height - tuiChromeHeight
	if m.tab == remoteTab {
		rows := m.remoteRows()
		for index := m.offset; index < len(rows) && remaining > 0; index++ {
			fmt.Fprintln(&view, m.remoteLine(rows[index], index == m.cursor))
			remaining--
		}
	} else if m.tab == skillsMPTab {
		for index := m.offset; index < len(m.skillsMPSkills) && remaining > 0; index++ {
			fmt.Fprintln(&view, m.skillsMPLine(m.skillsMPSkills[index], index == m.cursor))
			remaining--
		}
	} else {
		indices := m.localSkillIndices()
		for index := m.offset; index < len(indices) && remaining > 0; index++ {
			lines := m.skillLines(indices, index)
			if len(lines) > remaining {
				lines = lines[:remaining]
			}
			for _, line := range lines {
				fmt.Fprintln(&view, line)
			}
			remaining -= len(lines)
		}
	}
	help := "←/→ tabs • f filter • ↑/k ↓/j move • enter/click details • space toggle • e edit • q quit"
	if m.tab == remoteTab {
		help = "←/→ tabs • f filter • ↑/k ↓/j move • enter expand/collapse topic • q quit"
	} else if m.tab == skillsMPTab {
		help = "←/→ tabs • f search • ↑/k ↓/j move • q quit"
	}
	if m.filtering {
		help = "type to filter • enter/esc done"
	}
	fmt.Fprintf(
		&view,
		"%s\n%s\n",
		boundLine(
			mutedStyle.Render(help),
			m.width,
		),
		boundLine(statusStyle(m.status).Render(terminalSafeText(m.status)), m.width),
	)
	return view.String()
}

func (m model) filterLine() string {
	query := m.filterQuery
	if m.filtering {
		query += "▏"
	}
	line := " Filter: " + terminalSafeText(query)
	if m.filtering {
		return boundLine(selectedStyle(lipgloss.NewStyle(), true).Render(line), m.width)
	}
	return boundLine(mutedStyle.Render(line), m.width)
}

func (m model) tabBar() string {
	local := " Installed "
	remote := " skills.sh "
	skillsMP := " SkillsMP "
	if m.tab == localTab {
		local = selectedStyle(titleStyle, true).Render("[Installed]")
		remote = mutedStyle.Render(remote)
		skillsMP = mutedStyle.Render(skillsMP)
	} else if m.tab == remoteTab {
		local = mutedStyle.Render(local)
		remote = selectedStyle(titleStyle, true).Render("[skills.sh]")
		skillsMP = mutedStyle.Render(skillsMP)
	} else {
		local = mutedStyle.Render(local)
		remote = mutedStyle.Render(remote)
		skillsMP = selectedStyle(titleStyle, true).Render("[SkillsMP]")
	}
	return boundLine(local+" "+remote+" "+skillsMP, m.width)
}

type remoteRow struct {
	topic int
	skill int
	count int
}

func (m model) remoteRows() []remoteRow {
	rows := make([]remoteRow, 0, len(m.remoteTopics))
	query := strings.ToLower(m.filterQuery)
	for topicIndex, topic := range m.remoteTopics {
		topicMatches := matchesNormalizedFilter(
			query,
			topic.Name,
			topic.Slug,
		)
		matchingSkills := make([]int, 0, len(topic.Skills))
		for skillIndex, skill := range topic.Skills {
			if topicMatches || matchesNormalizedFilter(
				query,
				skill.Name,
				skill.ID,
				skill.Source,
			) {
				matchingSkills = append(matchingSkills, skillIndex)
			}
		}
		if query != "" && !topicMatches && len(matchingSkills) == 0 {
			continue
		}
		rows = append(rows, remoteRow{
			topic: topicIndex,
			skill: -1,
			count: len(matchingSkills),
		})
		if m.remoteCollapsed[topic.Slug] {
			continue
		}
		for _, skillIndex := range matchingSkills {
			rows = append(rows, remoteRow{topic: topicIndex, skill: skillIndex})
		}
	}
	return rows
}

func matchesNormalizedFilter(query string, values ...string) bool {
	if query == "" {
		return true
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func (m *model) toggleRemoteTopic() {
	rows := m.remoteRows()
	if m.cursor < 0 || m.cursor >= len(rows) || rows[m.cursor].skill >= 0 {
		return
	}
	topic := m.remoteTopics[rows[m.cursor].topic]
	if m.remoteCollapsed == nil {
		m.remoteCollapsed = make(map[string]bool)
	}
	m.remoteCollapsed[topic.Slug] = !m.remoteCollapsed[topic.Slug]
	m.syncViewport()
}

func (m *model) syncFixedRowViewport(count int) {
	if count == 0 {
		m.cursor = 0
		m.offset = 0
		return
	}
	m.cursor = min(max(m.cursor, 0), count-1)
	m.offset = min(max(m.offset, 0), count-1)
	visible := m.height - tuiChromeHeight
	if visible <= 0 {
		m.offset = m.cursor
		return
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	} else if m.cursor >= m.offset+visible {
		m.offset = m.cursor - visible + 1
	}
}

func (m model) skillsMPLine(skill skillsMPSkill, selected bool) string {
	name := selectedStyle(lipgloss.NewStyle(), selected).
		Render(terminalSafeText(skill.Name))
	label := fmt.Sprintf("%s • %d stars", terminalSafeText(skill.Author), skill.Stars)
	gap := max(m.width-lipgloss.Width(name)-lipgloss.Width(label), 1)
	line := name + selectedStyle(lipgloss.NewStyle(), selected).
		Render(strings.Repeat(" ", gap))
	line += selectedStyle(mutedStyle, selected).Render(label)
	return boundLine(line, m.width)
}

func (m model) remoteLine(row remoteRow, selected bool) string {
	topic := m.remoteTopics[row.topic]
	if row.skill < 0 {
		disclosure := "▾"
		if m.remoteCollapsed[topic.Slug] {
			disclosure = "▸"
		}
		line := selectedStyle(disclosureStyle, selected).Render(disclosure + " ")
		line += selectedStyle(lipgloss.NewStyle().Bold(true), selected).
			Render(terminalSafeText(topic.Name))
		line += selectedStyle(mutedStyle, selected).
			Render(fmt.Sprintf(" (%d)", row.count))
		return boundLine(line, m.width)
	}
	skill := topic.Skills[row.skill]
	name := selectedStyle(lipgloss.NewStyle(), selected).
		Render("  " + terminalSafeText(skill.Name))
	installs := fmt.Sprintf("%d installs", skill.Installs)
	source := terminalSafeText(skill.Source)
	label := source + " • " + installs
	gap := max(m.width-lipgloss.Width(name)-lipgloss.Width(label), 1)
	line := name + selectedStyle(lipgloss.NewStyle(), selected).
		Render(strings.Repeat(" ", gap))
	line += selectedStyle(mutedStyle, selected).Render(label)
	return boundLine(line, m.width)
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
	case strings.HasPrefix(status, "warning:"), strings.HasPrefix(status, "loading "):
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
	indices := m.localSkillIndices()
	for index := m.offset; index < len(indices) && remaining > 0; index++ {
		if y == row {
			return index, true
		}
		rendered := min(m.skillLineCount(indices, index), remaining)
		row += rendered
		remaining -= rendered
	}
	return 0, false
}

func (m model) rowCount(indices []int, start, end int) int {
	rows := 0
	for index := start; index < end; index++ {
		rows += m.skillLineCount(indices, index)
	}
	return rows
}

func (m model) skillLineCount(indices []int, index int) int {
	if index < 0 || index >= len(indices) {
		return 0
	}
	skill := m.skills[indices[index]]
	if m.expanded != skill.Name {
		return 1
	}
	return 1 +
		len(wrapDetail(skill.Description, m.width)) +
		pathLineCount(terminalSafeText(skill.Path), m.width)
}

func (m model) skillLines(indices []int, index int) []string {
	if index < 0 || index >= len(indices) {
		return nil
	}
	skill := m.skills[indices[index]]
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

func (m model) localSkillIndices() []int {
	indices := make([]int, 0, len(m.skills))
	query := strings.ToLower(m.filterQuery)
	for index, skill := range m.skills {
		if matchesNormalizedFilter(
			query,
			skill.Name,
			skill.Description,
			skill.Path,
			skill.Source,
		) {
			indices = append(indices, index)
		}
	}
	return indices
}

func (m model) localSkillIndex(index int) (int, bool) {
	indices := m.localSkillIndices()
	if index < 0 || index >= len(indices) {
		return 0, false
	}
	return indices[index], true
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
