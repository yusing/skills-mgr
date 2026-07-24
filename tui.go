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
	remoteSelected  map[string]bool
	registrySkills  []registrySearchSkill
	registryError   string
	registryCancel  context.CancelFunc
	registryRequest uint64
	tab             int
	cursor          int
	offset          int
	width           int
	height          int
	expanded        string
	busy            bool
	status          string
	progressTitle   string
	progressDetail  string
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
	registryDebounce = 300 * time.Millisecond
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

type remoteToggleDone struct {
	result remoteToggleResult
	err    error
}

type editDone struct {
	skill      string
	path       string
	skills     []discoveredSkill
	selected   map[string]bool
	editorErr  error
	refreshErr error
}

type registrySearchDone struct {
	request uint64
	tab     int
	query   string
	skills  []registrySearchSkill
	err     error
}

type registrySearchRequested struct {
	request uint64
	tab     int
	query   string
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
		remoteSelected:  remoteSelectionFrom(discovered, selected),
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
			m.remoteSelected = remoteSelectionFrom(m.skills, m.selected)
			if message.enabled {
				m.status = "enabled " + message.skill
			} else {
				m.status = "disabled " + message.skill
			}
		}
	case remoteToggleDone:
		m.busy = false
		m.progressTitle = ""
		m.progressDetail = ""
		if message.err != nil {
			m.status = "error: " + message.err.Error()
			break
		}
		m.skills = message.result.Skills
		m.selected = message.result.Selected
		m.remoteSelected = message.result.RemoteSelected
		if message.result.Enabled {
			m.status = "enabled " + message.result.Skill
		} else {
			m.status = "disabled " + message.result.Skill
		}
		m.syncViewport()
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
	case registrySearchDone:
		if message.request != m.registryRequest ||
			message.query != normalizedRegistryQuery(m.filterQuery) ||
			message.tab != m.tab {
			break
		}
		m.registryCancel = nil
		if message.err != nil {
			m.registrySkills = message.skills
			m.registryError = message.err.Error()
			if len(message.skills) == 0 {
				m.status = "error: " + message.err.Error()
			} else {
				m.status = "warning: " + message.err.Error()
			}
			break
		}
		m.registrySkills = message.skills
		m.registryError = ""
		m.status = fmt.Sprintf("%d %s skills", len(message.skills), registryName(message.tab))
		m.syncViewport()
	case registrySearchRequested:
		if message.request != m.registryRequest ||
			message.query != normalizedRegistryQuery(m.filterQuery) ||
			message.tab != m.tab {
			break
		}
		return m, m.startRegistrySearch(message.tab, message.query)
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
				m.cancelRegistryRequest()
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
			m.cancelRegistryRequest()
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
			return toggleSelectedSkill(m)
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

func toggleSelectedSkill(m model) (tea.Model, tea.Cmd) {
	if m.tab != localTab {
		ref, ok := remoteRefAtCursor(m)
		if !ok || m.manager == nil || m.manager.remoteStore == nil {
			return m, nil
		}
		enabled := m.remoteSelected[ref.key()]
		refresh := false
		status := "disabling "
		if !enabled {
			var err error
			refresh, err = m.manager.remoteStore.needsRefresh(ref)
			if err != nil {
				m.status = "error: " + err.Error()
				return m, nil
			}
			status = "enabling "
			if refresh {
				status = "installing "
			}
		}
		m.busy = true
		m.status = status + ref.Name
		if refresh {
			m.progressTitle = "Installing " + ref.Name
			m.progressDetail = "Cloning with git --depth 1…"
		}
		return m, func() tea.Msg {
			result, err := m.manager.toggleRemote(
				context.Background(),
				m.project,
				ref,
			)
			return remoteToggleDone{result: result, err: err}
		}
	}
	skillIndex, ok := m.localSkillIndex(m.cursor)
	if !ok {
		return m, nil
	}
	skill := m.skills[skillIndex].Name
	m.busy = true
	m.status = "updating " + skill
	return m, func() tea.Msg {
		enabled, err := m.manager.toggle(m.project, skill)
		return toggleDone{skill: skill, enabled: enabled, err: err}
	}
}

func (m *model) selectTab(tab int) tea.Cmd {
	if tab < localTab || tab > skillsMPTab || m.tab == tab {
		return nil
	}
	m.cancelRegistryRequest()
	m.tab = tab
	m.cursor = 0
	m.offset = 0
	switch tab {
	case remoteTab:
		if normalizedRegistryQuery(m.filterQuery) != "" {
			m.registrySkills = nil
			m.registryError = ""
			m.status = "searching skills.sh"
			m.syncViewport()
			return m.debounceRegistrySearch()
		}
		m.registrySkills = nil
		m.registryError = ""
		if m.manager != nil {
			m.reloadRemoteCache()
		}
		m.setRemoteStatus()
	case skillsMPTab:
		if normalizedRegistryQuery(m.filterQuery) != "" {
			m.registrySkills = nil
			m.registryError = ""
			m.status = "searching SkillsMP"
			m.syncViewport()
			return m.debounceRegistrySearch()
		}
		cache := m.reloadSkillsMPCache()
		if m.registryError == "" && len(cache.Skills) > 0 &&
			time.Now().Before(cache.UpdatedAt.Add(skillsMPCacheTTL)) {
			m.status = fmt.Sprintf("%d SkillsMP skills", len(m.registrySkills))
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
	m.cancelRegistryRequest()
	m.cursor = 0
	m.offset = 0
	m.expanded = ""
	if m.tab == remoteTab || m.tab == skillsMPTab {
		if normalizedRegistryQuery(m.filterQuery) == "" {
			m.registrySkills = nil
			m.registryError = ""
			if m.tab == remoteTab {
				if m.manager != nil {
					m.reloadRemoteCache()
				}
				m.setRemoteStatus()
				m.syncViewport()
				return nil
			}
			cache := m.reloadSkillsMPCache()
			if m.registryError == "" && len(cache.Skills) > 0 &&
				time.Now().Before(cache.UpdatedAt.Add(skillsMPCacheTTL)) {
				m.status = fmt.Sprintf("%d SkillsMP skills", len(m.registrySkills))
				return nil
			}
			m.status = "loading SkillsMP"
			return m.startSkillsMPCatalog()
		}
		m.registrySkills = nil
		m.registryError = ""
		m.status = "searching " + registryName(m.tab)
		return m.debounceRegistrySearch()
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

func (m *model) setRemoteStatus() {
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
}

func (m *model) reloadSkillsMPCache() skillsMPCache {
	if m.manager == nil {
		return skillsMPCache{}
	}
	cache, err := loadSkillsMPCache(m.manager.paths.skillsMP)
	if err != nil {
		m.registrySkills = nil
		m.registryError = err.Error()
		return skillsMPCache{}
	}
	m.registrySkills = presentSkillsMP(uniqueSkillsMP(cache.Skills))
	m.registryError = ""
	return cache
}

func (m *model) startRegistrySearch(tab int, query string) tea.Cmd {
	request := m.registryRequest
	ctx, cancel := context.WithCancel(context.Background())
	m.registryCancel = cancel
	return func() tea.Msg {
		defer cancel()
		provider := m.registryProvider(tab)
		if provider == nil {
			return registrySearchDone{
				request: request, tab: tab, query: query,
				err: fmt.Errorf("%s provider is unavailable", registryName(tab)),
			}
		}
		skills, err := provider.search(ctx, query)
		return registrySearchDone{
			request: request, tab: tab, query: query, skills: skills, err: err,
		}
	}
}

func (m model) registryProvider(tab int) registrySearchProvider {
	if m.manager == nil {
		return nil
	}
	switch tab {
	case remoteTab:
		return m.manager.remote
	case skillsMPTab:
		return m.manager.skillsMP
	default:
		return nil
	}
}

func registryName(tab int) string {
	if tab == remoteTab {
		return "skills.sh"
	}
	if tab == skillsMPTab {
		return "SkillsMP"
	}
	return "registry"
}

func (m *model) startSkillsMPCatalog() tea.Cmd {
	request := m.registryRequest
	ctx, cancel := context.WithCancel(context.Background())
	m.registryCancel = cancel
	return func() tea.Msg {
		defer cancel()
		if m.manager == nil || m.manager.skillsMP == nil {
			return registrySearchDone{
				request: request, tab: skillsMPTab,
				err: fmt.Errorf("SkillsMP provider is unavailable"),
			}
		}
		skills, err := m.manager.skillsMP.catalog(ctx)
		return registrySearchDone{
			request: request, tab: skillsMPTab,
			skills: presentSkillsMP(skills), err: err,
		}
	}
}

func (m *model) cancelRegistryRequest() {
	m.registryRequest++
	if m.registryCancel != nil {
		m.registryCancel()
		m.registryCancel = nil
	}
}

func (m model) debounceRegistrySearch() tea.Cmd {
	request := m.registryRequest
	tab := m.tab
	query := normalizedRegistryQuery(m.filterQuery)
	return tea.Tick(registryDebounce, func(time.Time) tea.Msg {
		return registrySearchRequested{request: request, tab: tab, query: query}
	})
}

func (m model) itemCount() int {
	if m.tab == remoteTab {
		if normalizedRegistryQuery(m.filterQuery) != "" {
			return len(m.registrySkills)
		}
		return len(m.remoteRows())
	}
	if m.tab == skillsMPTab {
		return len(m.registrySkills)
	}
	return len(m.localSkillIndices())
}

func remoteRefAtCursor(m model) (remoteSkillRef, bool) {
	switch {
	case m.tab == remoteTab && normalizedRegistryQuery(m.filterQuery) == "":
		rows := m.remoteRows()
		if m.cursor < 0 || m.cursor >= len(rows) || rows[m.cursor].skill < 0 {
			return remoteSkillRef{}, false
		}
		row := rows[m.cursor]
		return m.remoteTopics[row.topic].Skills[row.skill].ref(), true
	case m.tab == remoteTab || m.tab == skillsMPTab:
		if m.cursor < 0 || m.cursor >= len(m.registrySkills) {
			return remoteSkillRef{}, false
		}
		return m.registrySkills[m.cursor].ref(), true
	default:
		return remoteSkillRef{}, false
	}
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
		if normalizedRegistryQuery(m.filterQuery) != "" {
			m.syncFixedRowViewport(len(m.registrySkills))
			return
		}
		m.syncFixedRowViewport(len(m.remoteRows()))
		return
	}
	if m.tab == skillsMPTab {
		m.syncFixedRowViewport(len(m.registrySkills))
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
	if m.tab == remoteTab && normalizedRegistryQuery(m.filterQuery) == "" {
		rows := m.remoteRows()
		for index := m.offset; index < len(rows) && remaining > 0; index++ {
			fmt.Fprintln(&view, m.remoteLine(rows[index], index == m.cursor))
			remaining--
		}
	} else if m.tab == remoteTab || m.tab == skillsMPTab {
		for index := m.offset; index < len(m.registrySkills) && remaining > 0; index++ {
			fmt.Fprintln(&view, m.registrySkillLine(m.registrySkills[index], index == m.cursor))
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
		help = "←/→ tabs • f search • ↑/k ↓/j move • enter topics • space toggle • q quit"
	} else if m.tab == skillsMPTab {
		help = "←/→ tabs • f search • ↑/k ↓/j move • space toggle • q quit"
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
	if m.progressTitle == "" {
		return view.String()
	}
	return m.progressPopup()
}

func (m model) progressPopup() string {
	width := min(max(m.width-4, 20), 72)
	content := titleStyle.Render(terminalSafeText(m.progressTitle)) + "\n\n" +
		terminalSafeText(m.progressDetail)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accentColor).
		Padding(1, 2).
		Width(width).
		Render(content)
	return lipgloss.Place(
		max(m.width, lipgloss.Width(box)),
		max(m.height, lipgloss.Height(box)),
		lipgloss.Center,
		lipgloss.Center,
		box,
	)
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
	for topicIndex, topic := range m.remoteTopics {
		rows = append(rows, remoteRow{
			topic: topicIndex,
			skill: -1,
			count: len(topic.Skills),
		})
		if m.remoteCollapsed[topic.Slug] {
			continue
		}
		for skillIndex := range topic.Skills {
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
	if normalizedRegistryQuery(m.filterQuery) != "" {
		return
	}
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

func (m model) registrySkillLine(skill registrySearchSkill, selected bool) string {
	displayName := skill.Name
	if m.remoteSelected[skill.ref().key()] {
		displayName += " [enabled]"
	}
	name := selectedStyle(lipgloss.NewStyle(), selected).
		Render(terminalSafeText(displayName))
	label := terminalSafeText(skill.Label)
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
	displayName := skill.Name
	if m.remoteSelected[skill.ref().key()] {
		displayName += " [enabled]"
	}
	name := selectedStyle(lipgloss.NewStyle(), selected).
		Render("  " + terminalSafeText(displayName))
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
	case strings.HasPrefix(status, "warning:"),
		strings.HasPrefix(status, "loading "),
		strings.HasPrefix(status, "fetching "),
		strings.HasPrefix(status, "enabling "),
		strings.HasPrefix(status, "disabling "):
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
	for _, enabled := range []bool{true, false} {
		for index, skill := range m.skills {
			if m.selected[skill.Name] != enabled {
				continue
			}
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
