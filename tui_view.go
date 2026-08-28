package main

import (
	"fmt"

	"strings"

	"unicode"

	"github.com/charmbracelet/lipgloss"
)

func (m *model) toggleCurrentExpanded(index int) {
	if isLocalTab(m.tab) {
		skillIndex, ok := m.localSkillIndex(index)
		if !ok {
			return
		}
		m.toggleCollapsible(index, m.skills[skillIndex].Name)
		return
	}
	if m.tab == remoteTab && normalizedRegistryQuery(m.filterQuery) == "" {
		rows := m.remoteRows()
		if index < 0 || index >= len(rows) {
			return
		}
		row := rows[index]
		if row.skill < 0 {
			m.cursor = index
			m.toggleRemoteTopic()
			return
		}
		m.toggleCollapsible(
			index,
			m.remoteTopics[row.topic].Skills[row.skill].ref().key(),
		)
		return
	}
	if index < 0 || index >= len(m.registrySkills) {
		return
	}
	m.toggleCollapsible(index, m.registrySkills[index].ref().key())
}

func (m *model) toggleCollapsible(index int, key string) {
	m.cursor = index
	if m.expanded == key {
		m.expanded = ""
	} else {
		m.expanded = key
	}
	m.syncViewport()
}

type renderedList struct {
	count int
	lines func(index int) []string
}

func newRenderedList(count int, render func(index int) []string) renderedList {
	var cache map[int][]string
	return renderedList{
		count: count,
		lines: func(index int) []string {
			if lines, ok := cache[index]; ok {
				return lines
			}
			if cache == nil {
				cache = make(map[int][]string)
			}
			lines := render(index)
			cache[index] = lines
			return lines
		},
	}
}

func (m model) currentList() renderedList {
	switch {
	case m.tab == remoteTab && normalizedRegistryQuery(m.filterQuery) == "":
		rows := m.remoteRows()
		return newRenderedList(
			len(rows),
			func(index int) []string {
				return m.remoteLines(rows[index], index == m.cursor)
			},
		)
	case m.tab == remoteTab || m.tab == skillsMPTab:
		return newRenderedList(
			len(m.registrySkills),
			func(index int) []string {
				return m.registrySkillLines(
					m.registrySkills[index],
					index == m.cursor,
				)
			},
		)
	default:
		indices := m.localSkillIndices()
		return newRenderedList(
			len(indices),
			func(index int) []string {
				return m.skillLines(indices, index)
			},
		)
	}
}

// currentRowCount reports the active tab's row count without building the
// render closure or its cache, which the cursor-clamping callers never use.
func (m model) currentRowCount() int {
	switch {
	case m.tab == remoteTab && normalizedRegistryQuery(m.filterQuery) == "":
		return len(m.remoteRows())
	case m.tab == remoteTab || m.tab == skillsMPTab:
		return len(m.registrySkills)
	default:
		return len(m.localSkillIndices())
	}
}

func (m *model) syncViewport() renderedList {
	count := m.currentRowCount()
	if count == 0 {
		m.cursor = 0
		m.offset = 0
		return m.currentList()
	}
	// currentList captures the cursor, so clamp before building the list.
	m.cursor = min(max(m.cursor, 0), count-1)
	m.offset = min(max(m.offset, 0), count-1)
	list := m.currentList()
	visible := m.height - m.chromeHeight()
	if visible <= 0 {
		m.offset = m.cursor
		return list
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	for m.offset < m.cursor &&
		listLineCount(list, m.offset, m.cursor+1) > visible {
		m.offset++
	}
	for m.offset > 0 &&
		listLineCount(list, m.offset-1, m.cursor+1) <= visible {
		m.offset--
	}
	return list
}

func listLineCount(list renderedList, start, end int) int {
	rows := 0
	for index := start; index < end; index++ {
		rows += len(list.lines(index))
	}
	return rows
}

func (m model) View() string {
	list := m.syncViewport()
	var view strings.Builder
	fmt.Fprintf(
		&view,
		"\n%s\n%s\n%s\n",
		boundLine(titleStyle.Render("Skill Manager"), m.width),
		boundLine(mutedStyle.Render(terminalSafeText(m.project)), m.width),
		m.tabBar(),
	)
	if len(sourceSubtabsFor(m.tab)) > 0 {
		fmt.Fprintln(&view, m.sourceSubtabBar())
	}
	fmt.Fprintln(&view, m.filterLine())
	remaining := m.height - m.chromeHeight()
	for index := m.offset; index < list.count && remaining > 0; index++ {
		lines := list.lines(index)
		if len(lines) > remaining {
			lines = lines[:remaining]
		}
		for _, line := range lines {
			fmt.Fprintln(&view, line)
		}
		remaining -= len(lines)
	}
	help := "←/→ tabs • f filter • ↑/k ↓/j move • enter/click details • space toggle • m model toggle • e source • i enabled • q quit"
	switch m.tab {
	case localTab:
		help = "←/→ tabs • f filter • ↑/k ↓/j move • enter/click details • space toggle • m model toggle • a adopt/release • u uninstall • e source • i enabled • q quit"
	case codexTab:
		help = "←/→ tabs • [] sources • f filter • ↑/k ↓/j move • enter/click details • space toggle • m model toggle • e source • i enabled • q quit"
	case grokTab:
		if m.sourceSubtabs[grokTab] == userSourceSubtab {
			help = "←/→ tabs • [] sources • f filter • ↑/k ↓/j move • enter/click details • space toggle • m model toggle • e source • i enabled • q quit"
		} else {
			help = "←/→ tabs • [] sources • f filter • ↑/k ↓/j move • enter/click details • space global toggle • q quit"
		}
	case claudeTab:
		if m.sourceSubtabs[claudeTab] == userSourceSubtab {
			help = "←/→ tabs • [] sources • f filter • ↑/k ↓/j move • enter/click details • space toggle • m model toggle • e source • i enabled • q quit"
		} else {
			help = "←/→ tabs • [] sources • f filter • ↑/k ↓/j move • enter/click details • plugin status display only • q quit"
		}
	case remoteTab:
		help = "←/→ tabs • f search • ↑/k ↓/j move • enter/click details/topics • space toggle • q quit"
	case skillsMPTab:
		help = "←/→ tabs • f search • ↑/k ↓/j move • enter/click details • space toggle • q quit"
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

func styledTab(label string, selected bool) string {
	if selected {
		return selectedStyle(titleStyle, true).Render("[" + label + "]")
	}
	return mutedStyle.Render(" " + label + " ")
}

func (m model) tabBar() string {
	parts := make([]string, 0, 6)
	for _, tab := range []struct {
		id    int
		label string
	}{
		{localTab, "Installed"},
		{codexTab, "Codex"},
		{grokTab, "Grok"},
		{claudeTab, "Claude"},
		{remoteTab, "skills.sh"},
		{skillsMPTab, "SkillsMP"},
	} {
		parts = append(parts, styledTab(tab.label, m.tab == tab.id))
	}
	return boundLine(strings.Join(parts, " "), m.width)
}

func (m model) sourceSubtabBar() string {
	subtabs := sourceSubtabsFor(m.tab)
	parts := make([]string, 0, len(subtabs))
	for id, subtab := range subtabs {
		parts = append(parts, styledTab(subtab.label, m.sourceSubtabs[m.tab] == id))
	}
	return boundLine(strings.Join(parts, " "), m.width)
}

func (m model) sourceSubtabAt(x int) (int, bool) {
	col := 0
	for id, subtab := range sourceSubtabsFor(m.tab) {
		part := styledTab(subtab.label, m.sourceSubtabs[m.tab] == id)
		width := lipgloss.Width(part)
		if id > 0 {
			col++
		}
		if x >= col && x < col+width {
			return id, true
		}
		col += width
	}
	return 0, false
}

type remoteRow struct {
	topic int
	skill int
	count int
}

func (m model) remoteRows() []remoteRow {
	total := len(m.remoteTopics)
	for _, topic := range m.remoteTopics {
		if !m.remoteCollapsed[topic.Slug] {
			total += len(topic.Skills)
		}
	}
	rows := make([]remoteRow, 0, total)
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

func (m model) registrySkillLines(skill registrySearchSkill, selected bool) []string {
	displayName := skill.Name
	if m.remoteSelected[skill.ref().key()] {
		displayName += " [enabled]"
	}
	expanded := m.expanded == skill.ref().key()
	disclosure := disclosureIndicator(expanded)
	name := selectedStyle(disclosureStyle, selected).Render(disclosure + " ")
	name += selectedStyle(lipgloss.NewStyle(), selected).
		Render(terminalSafeText(displayName))
	label := terminalSafeText(skill.Label)
	header := labeledListLine(name, label, mutedStyle, selected, m.width)
	return collapsibleLines(header, expanded, func() []string {
		return remoteDetailLines(
			skill.Description,
			skill.Provider,
			skill.Locator,
			m.width,
		)
	})
}

func (m model) remoteLines(row remoteRow, selected bool) []string {
	topic := m.remoteTopics[row.topic]
	if row.skill < 0 {
		disclosure := disclosureIndicator(!m.remoteCollapsed[topic.Slug])
		line := selectedStyle(disclosureStyle, selected).Render(disclosure + " ")
		line += selectedStyle(lipgloss.NewStyle().Bold(true), selected).
			Render(terminalSafeText(topic.Name))
		line += selectedStyle(mutedStyle, selected).
			Render(fmt.Sprintf(" (%d)", row.count))
		return []string{boundLine(line, m.width)}
	}
	skill := topic.Skills[row.skill]
	displayName := skill.Name
	if m.remoteSelected[skill.ref().key()] {
		displayName += " [enabled]"
	}
	expanded := m.expanded == skill.ref().key()
	disclosure := disclosureIndicator(expanded)
	name := selectedStyle(lipgloss.NewStyle(), selected).Render("  ")
	name += selectedStyle(disclosureStyle, selected).Render(disclosure + " ")
	name += selectedStyle(lipgloss.NewStyle(), selected).
		Render(terminalSafeText(displayName))
	installs := fmt.Sprintf("%d installs", skill.Installs)
	source := terminalSafeText(skill.Source)
	label := source + " • " + installs
	header := labeledListLine(name, label, mutedStyle, selected, m.width)
	return collapsibleLines(header, expanded, func() []string {
		return remoteDetailLines(
			skill.Description,
			skillsShProvider,
			skill.ID,
			m.width,
		)
	})
}

func disclosureIndicator(expanded bool) string {
	if expanded {
		return "▾"
	}
	return "▸"
}

func collapsibleLines(
	header string,
	expanded bool,
	details func() []string,
) []string {
	lines := []string{header}
	if expanded {
		lines = append(lines, details()...)
	}
	return lines
}

func labeledListLine(
	name, label string,
	labelStyle lipgloss.Style,
	selected bool,
	width int,
) string {
	gap := max(width-lipgloss.Width(name)-lipgloss.Width(label), 1)
	line := name + selectedStyle(lipgloss.NewStyle(), selected).
		Render(strings.Repeat(" ", gap))
	line += selectedStyle(labelStyle, selected).Render(label)
	return boundLine(line, width)
}

func remoteDetailLines(description, provider, locator string, width int) []string {
	lines := descriptionLines(description, width)
	if locator != "" {
		label := "id"
		if provider == skillsMPProvider {
			label = "url"
		}
		lines = append(lines, styledMetadataLines(label, terminalSafeText(locator), width)...)
	}
	return lines
}

func descriptionLines(description string, width int) []string {
	if description == "" {
		return nil
	}
	lines := make([]string, 0)
	for _, detail := range wrapDetail(terminalSafeText(description), width) {
		lines = append(lines, boundLine(mutedStyle.Render(detail), width))
	}
	return lines
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

func (m model) itemIndexAtHeaderRow(y int) (int, bool) {
	header := m.headerHeight()
	if y < header || y >= m.height-tuiFooterHeight {
		return 0, false
	}
	list := m.currentList()
	row := header
	remaining := m.height - m.chromeHeight()
	for index := m.offset; index < list.count && remaining > 0; index++ {
		if y == row {
			return index, true
		}
		rendered := min(len(list.lines(index)), remaining)
		row += rendered
		remaining -= rendered
	}
	return 0, false
}

func (m model) skillLines(indices []int, index int) []string {
	if index < 0 || index >= len(indices) {
		return nil
	}
	skill := m.skills[indices[index]]
	cursor := " "
	if index == m.cursor {
		cursor = ">"
	}
	expanded := m.expanded == skill.Name
	disclosure := disclosureIndicator(expanded)
	source := skillSourceLabel(skill.Source)
	if source == "" {
		source = "unknown"
	}
	label := "(" + source + ")"
	selected := index == m.cursor
	name := selectedStyle(lipgloss.NewStyle(), selected).Render(cursor + " ")
	name += selectedStyle(disclosureStyle, selected).Render(disclosure + " ")
	name += selectedStyle(lipgloss.NewStyle(), selected).Render(skill.Name)
	if !isNativeSkill(skill) && m.inherited(skill.Name) {
		name += selectedStyle(inheritedStyle, selected).Render(" [inherited]")
	}
	if skill.DisableModelInvocation {
		name += selectedStyle(manualOnlyStyle, selected).Render(" [manual-only]")
	}
	_, projectConditional := m.projectConditional[skill.Name]
	_, globalConditional := m.globalConditional[skill.Name]
	_, projectConfigured := m.projectSelected[skill.Name]
	if isNativeSkill(skill) {
		projectConditional = false
		globalConditional = false
		projectConfigured = true
	}
	effectiveGlobalConditional := globalConditional && !projectConfigured
	if projectConditional || effectiveGlobalConditional {
		label := " [conditional]"
		if !projectConditional {
			if effectiveGlobalConditional && m.projectSelected != nil {
				label = " [conditional inherited]"
			}
		}
		name += selectedStyle(manualOnlyStyle, selected).Render(label)
	}
	if isNativeSkill(skill) && skill.ExternalEnabled {
		name += selectedStyle(lipgloss.NewStyle().Bold(true).Foreground(successColor), selected).
			Render(" [enabled]")
	} else if !m.skillEnabled(skill) {
		name += selectedStyle(disabledStyle, selected).Render(" [disabled]")
	}
	header := labeledListLine(name, label, skillSourceStyle(skill), selected, m.width)
	return collapsibleLines(header, expanded, func() []string {
		details := descriptionLines(skill.Description, m.width)
		if skill.Plugin != "" {
			details = append(details, styledMetadataLines("plugin", terminalSafeText(skill.Plugin), m.width)...)
		}
		if skill.Vendor != "" {
			details = append(details, styledMetadataLines("vendor", terminalSafeText(skill.Vendor), m.width)...)
		}
		if isGrokNativeSkill(skill) {
			details = append(details, styledMetadataLines("user invocable", fmt.Sprintf("%t", skill.UserInvocable), m.width)...)
		}
		if skill.CompatibilityStatus != "" {
			details = append(details, styledMetadataLines("compatibility", terminalSafeText(skill.CompatibilityStatus), m.width)...)
		}
		return append(
			details,
			styledMetadataLines("path", terminalSafeText(skill.Path), m.width)...,
		)
	})
}

func skillSourceLabel(source string) string {
	switch source {
	case claudePluginSource, grokPluginSource:
		return "plugin"
	case grokBundledSource:
		return "bundled"
	default:
		return source
	}
}

func skillSourceStyle(skill discoveredSkill) lipgloss.Style {
	color := mutedColor
	switch {
	case skill.RemoteKey != "",
		skill.Source == skillsShProvider,
		skill.Source == skillsMPProvider:
		color = inheritedColor
	case skill.Source == "user":
		color = accentColor
	case skill.Source == managedSkillSource:
		color = successColor
	case skill.Source == "plugin", skill.Source == claudePluginSource, skill.Source == grokPluginSource:
		color = warningColor
	case skill.Source == "bundled", skill.Source == grokBundledSource:
		color = lipgloss.AdaptiveColor{Light: "#A63D73", Dark: "#F49AC2"}
	}
	return lipgloss.NewStyle().Foreground(color)
}

func (m model) localSkillIndices() []int {
	// Grouped in index order, groups concatenated: one classification pass keeps
	// that ordering without asking localSkillGroup about every skill four times.
	var groups [4][]int
	query := strings.ToLower(m.filterQuery)
	for index, skill := range m.skills {
		if !matchesNormalizedFilter(
			query,
			skill.Name,
			skill.Description,
			skill.Path,
			skill.Source,
			skill.Plugin,
			skill.Vendor,
			skill.CompatibilityStatus,
		) {
			continue
		}
		group := m.localSkillGroup(skill)
		groups[group] = append(groups[group], index)
	}
	indices := make([]int, 0, len(m.skills))
	for _, group := range groups {
		indices = append(indices, group...)
	}
	return indices
}

func (m model) localSkillGroup(skill discoveredSkill) int {
	if isNativeSkill(skill) {
		if skill.ExternalEnabled {
			return 0
		}
		return 3
	}
	name := skill.Name
	if m.projectSelected == nil {
		if m.selected[name] {
			return 0
		}
		return 3
	}
	projectEnabled, projectExists := m.projectSelected[name]
	globalEnabled := m.globalSelected[name]
	switch {
	case projectExists && projectEnabled:
		return 0
	case projectExists && !projectEnabled && globalEnabled:
		return 1
	case !projectExists && globalEnabled:
		return 2
	default:
		return 3
	}
}

func (m model) skillEnabled(skill discoveredSkill) bool {
	if isNativeSkill(skill) {
		return skill.ExternalEnabled
	}
	return m.selected[skill.Name]
}

func (m model) inherited(skill string) bool {
	if m.projectSelected == nil {
		return false
	}
	if _, exists := m.projectSelected[skill]; exists {
		return false
	}
	return m.globalSelected[skill]
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

func styledMetadataLines(label, value string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	prefix := metadataPrefix(label, width)
	available := width - lipgloss.Width(prefix)
	chunks := hardWrap(value, available)
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

func metadataPrefix(label string, width int) string {
	field := label + "  "
	if width <= lipgloss.Width(field) {
		return ""
	}
	indent := strings.Repeat(" ", min(4, width-lipgloss.Width(field)-1))
	return indent + field
}

func hardWrap(value string, width int) []string {
	if value == "" || width <= 0 {
		return []string{value}
	}
	var lines []string
	runes := []rune(value)
	for lipgloss.Width(string(runes)) > width {
		cut := displayWidthCut(runes, width)
		lines = append(lines, string(runes[:cut]))
		runes = runes[cut:]
	}
	return append(lines, string(runes))
}

// displayWidthCut reports how many leading runes fit in width columns, always at
// least one so callers keep making progress. Every value reaching the wrappers
// has passed through terminalSafeText, which strips control runes including ESC,
// so no escape sequence can make a longer prefix measure narrower; prefix width
// is therefore non-decreasing and this binary search is exact.
func displayWidthCut(runes []rune, width int) int {
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		if lipgloss.Width(string(runes[:mid])) <= width {
			low = mid
		} else {
			high = mid - 1
		}
	}
	if low == 0 {
		return 1
	}
	return low
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
