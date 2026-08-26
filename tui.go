package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-shellwords"
	"golang.org/x/term"
)

//nolint:recvcheck // Bubble Tea's value-returning model uses pointer helpers on its local copy.
type model struct {
	manager            *manager
	project            string
	skills             []discoveredSkill
	selected           map[string]bool
	globalSelected     map[string]bool
	projectSelected    map[string]bool
	globalConditional  map[string]string
	projectConditional map[string]string
	remoteTopics       []remoteTopic
	remoteCollapsed    map[string]bool
	remoteError        string
	remoteSelected     map[string]bool
	registrySkills     []registrySearchSkill
	registryError      string
	registryCancel     context.CancelFunc
	registryRequest    uint64
	tab                int
	cursor             int
	offset             int
	width              int
	height             int
	expanded           string
	busy               bool
	busyCancel         context.CancelFunc
	quitAfterBusy      bool
	status             string
	progressTitle      string
	progressDetail     string
	filtering          bool
	filterQuery        string
	allSkills          []discoveredSkill
	sourceSubtabs      [claudeTab + 1]int
}

const (
	tuiHeaderHeight     = 5
	tuiFooterHeight     = 2
	localTab            = 0
	codexTab            = 1
	grokTab             = 2
	claudeTab           = 3
	remoteTab           = 4
	skillsMPTab         = 5
	userSourceSubtab    = 0
	pluginSourceSubtab  = 1
	bundledSourceSubtab = 2
	systemSourceSubtab  = 3
	registryDebounce    = 300 * time.Millisecond
)

type sourceSubtab struct {
	label  string
	source string
}

var (
	codexSourceSubtabs = []sourceSubtab{
		{label: "User", source: "codex"},
		{label: "Plugin", source: "plugin"},
		{label: "Builtin", source: "bundled"},
		{label: "System", source: "admin"},
	}
	grokSourceSubtabs = []sourceSubtab{
		{label: "User", source: "grok"},
		{label: "Plugin", source: grokPluginSource},
		{label: "Bundled", source: grokBundledSource},
	}
	claudeSourceSubtabs = []sourceSubtab{
		{label: "User", source: "claude"},
		{label: "Plugin", source: claudePluginSource},
	}
)

var (
	accentColor    = lipgloss.AdaptiveColor{Light: "#005F87", Dark: "#7DD3FC"}
	disabledColor  = lipgloss.AdaptiveColor{Light: "#C01C4A", Dark: "#FF7A90"}
	mutedColor     = lipgloss.AdaptiveColor{Light: "#626262", Dark: "#808080"}
	successColor   = lipgloss.AdaptiveColor{Light: "#237A3B", Dark: "#75D18B"}
	warningColor   = lipgloss.AdaptiveColor{Light: "#8A5A00", Dark: "#E5C07B"}
	errorColor     = lipgloss.AdaptiveColor{Light: "#B42318", Dark: "#FF6B6B"}
	selectedColor  = lipgloss.AdaptiveColor{Light: "#DCEEFF", Dark: "#30363D"}
	inheritedColor = lipgloss.AdaptiveColor{Light: "#6F42C1", Dark: "#C4A7E7"}

	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(accentColor)
	mutedStyle      = lipgloss.NewStyle().Foreground(mutedColor)
	disclosureStyle = lipgloss.NewStyle().Foreground(accentColor)
	disabledStyle   = lipgloss.NewStyle().Bold(true).Foreground(disabledColor)
	inheritedStyle  = lipgloss.NewStyle().Bold(true).Foreground(inheritedColor)
	manualOnlyStyle = lipgloss.NewStyle().Bold(true).Foreground(warningColor)
	pathStyle       = lipgloss.NewStyle().Foreground(accentColor).Underline(true)
)

type toggleDone struct {
	skill   string
	enabled bool
	err     error
}

type grokToggleDone struct {
	skill   string
	enabled bool
	err     error
}

type remoteToggleDone struct {
	result remoteToggleResult
	err    error
}

type remoteUninstallDone struct {
	result remoteUninstallResult
	err    error
}

type modelInvocationDone struct {
	result modelInvocationResult
	err    error
}

type skillLocationDone struct {
	result skillLocationResult
	err    error
}

type editDone struct {
	skill      string
	path       string
	skills     []discoveredSkill
	selection  selectionState
	editorErr  error
	refreshErr error
}

type skillEdit struct {
	command       *exec.Cmd
	draft         string
	layeredDigest [sha256.Size]byte
	source        discoveredSkill
}

type enabledEditDone struct {
	skill      string
	selection  selectionState
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
	native, err := manager.nativeSkills(project)
	if err != nil {
		return model{}, err
	}
	discovered = append(discovered, native...)
	slices.SortFunc(discovered, compareDiscoveredSkills)
	selection, err := manager.selectionState(project, nil)
	if err != nil {
		return model{}, err
	}
	current := model{
		manager: manager, project: project, allSkills: discovered,
		selected:           selection.selected,
		globalSelected:     selection.globalSelected,
		projectSelected:    selection.projectSelected,
		globalConditional:  selection.globalExpressions,
		projectConditional: selection.projectExpressions,
		remoteCollapsed:    make(map[string]bool),
		remoteSelected:     remoteSelectionFrom(discovered, selection.selected),
	}
	current.applyCatalog()
	current.status = fmt.Sprintf("%d skills", len(current.skills))
	current.reloadRemoteCache()
	return current, nil
}

func isLocalTab(tab int) bool {
	switch tab {
	case localTab, codexTab, grokTab, claudeTab:
		return true
	default:
		return false
	}
}

func (m model) catalogHarnesses() []listHarness {
	switch m.tab {
	case codexTab:
		return []listHarness{listHarnessCodex}
	case grokTab:
		return []listHarness{listHarnessGrok}
	case claudeTab:
		return []listHarness{listHarnessClaude}
	default:
		return nil
	}
}

func (m *model) applyCatalog() {
	skills := catalogSkills(m.allSkills, m.catalogHarnesses())
	if subtabs := sourceSubtabsFor(m.tab); len(subtabs) > 0 {
		skills = filterAgentSource(skills, subtabs[m.sourceSubtabs[m.tab]].source)
	}
	m.skills = skills
}

func (m model) headerHeight() int {
	if len(sourceSubtabsFor(m.tab)) > 0 {
		return tuiHeaderHeight + 1
	}
	return tuiHeaderHeight
}

func (m model) chromeHeight() int {
	return m.headerHeight() + tuiFooterHeight
}

func filterAgentSource(skills []discoveredSkill, want string) []discoveredSkill {
	filtered := make([]discoveredSkill, 0, len(skills))
	for _, skill := range skills {
		if skill.Source == want {
			filtered = append(filtered, skill)
		}
	}
	return filtered
}

func sourceSubtabsFor(tab int) []sourceSubtab {
	switch tab {
	case codexTab:
		return codexSourceSubtabs
	case grokTab:
		return grokSourceSubtabs
	case claudeTab:
		return claudeSourceSubtabs
	default:
		return nil
	}
}

func compareDiscoveredSkills(a, b discoveredSkill) int {
	if byName := strings.Compare(a.Name, b.Name); byName != 0 {
		return byName
	}
	return strings.Compare(a.Source, b.Source)
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
			if m.projectSelected != nil {
				m.projectSelected[message.skill] = message.enabled
				delete(m.projectConditional, message.skill)
			} else {
				delete(m.globalConditional, message.skill)
			}
			catalog := m.allSkills
			if catalog == nil {
				catalog = m.skills
			}
			m.remoteSelected = remoteSelectionFrom(catalog, m.selected)
			if message.enabled {
				m.status = "enabled " + message.skill
			} else {
				m.status = "disabled " + message.skill
			}
		}
	case grokToggleDone:
		m.busy = false
		if message.err != nil {
			m.status = "error: " + message.err.Error()
			break
		}
		for index := range m.allSkills {
			skill := &m.allSkills[index]
			if skill.Name == message.skill && isGrokNativeSkill(*skill) {
				skill.ExternalEnabled = message.enabled
			}
		}
		m.applyCatalog()
		if message.enabled {
			m.status = "enabled " + message.skill + " in Grok"
		} else {
			m.status = "disabled " + message.skill + " in Grok"
		}
	case remoteToggleDone:
		m.busy = false
		m.progressTitle = ""
		m.progressDetail = ""
		if message.err != nil {
			m.status = "error: " + message.err.Error()
			break
		}
		m.replaceManagedSkills(message.result.Skills)
		m.applyCatalog()
		m.selected = message.result.Selected
		if m.projectSelected != nil {
			m.projectSelected[message.result.Skill] = message.result.Enabled
			delete(m.projectConditional, message.result.Skill)
		} else {
			delete(m.globalConditional, message.result.Skill)
		}
		m.remoteSelected = message.result.RemoteSelected
		if message.result.Enabled {
			m.status = "enabled " + message.result.Skill
		} else {
			m.status = "disabled " + message.result.Skill
		}
		m.syncViewport()
	case remoteUninstallDone:
		m.busy = false
		if m.busyCancel != nil {
			m.busyCancel()
			m.busyCancel = nil
		}
		if message.err != nil {
			m.status = "error: " + message.err.Error()
			break
		}
		m.replaceManagedSkills(message.result.Skills)
		m.applyCatalog()
		m.selected = message.result.Selected
		m.globalSelected = message.result.GlobalSelected
		m.projectSelected = message.result.ProjectSelected
		if m.projectSelected != nil {
			delete(m.projectConditional, message.result.Skill)
		} else {
			delete(m.globalConditional, message.result.Skill)
		}
		m.remoteSelected = remoteSelectionFrom(m.allSkills, m.selected)
		if m.expanded == message.result.Skill {
			m.expanded = ""
		}
		m.status = "uninstalled " + message.result.Skill
		m.syncViewport()
	case skillLocationDone:
		m.busy = false
		if message.err != nil {
			m.status = "error: " + message.err.Error()
			break
		}
		m.replaceManagedSkills(message.result.Skills)
		m.applyCatalog()
		m.selected = message.result.Selected
		m.globalSelected = message.result.GlobalSelected
		m.projectSelected = message.result.ProjectSelected
		m.remoteSelected = remoteSelectionFrom(m.allSkills, m.selected)
		m.expanded = ""
		where := "released to .agents/skills"
		if message.result.Managed {
			where = "adopted into the manager home"
		}
		m.status = message.result.Skill + " " + where
	case modelInvocationDone:
		m.busy = false
		if m.busyCancel != nil {
			m.busyCancel()
			m.busyCancel = nil
		}
		if message.err != nil {
			m.status = "error: " + message.err.Error()
			break
		}
		wasExpanded := m.expanded == message.result.Skill
		m.replaceManagedSkills(message.result.Skills)
		m.applyCatalog()
		m.selected = message.result.Selected
		m.globalSelected = message.result.GlobalSelected
		m.projectSelected = message.result.ProjectSelected
		m.remoteSelected = remoteSelectionFrom(m.allSkills, m.selected)
		m.expanded = ""
		for index, skillIndex := range m.localSkillIndices() {
			skill := m.skills[skillIndex]
			if skill.Name != message.result.Skill ||
				skill.RemoteKey != message.result.RemoteKey {
				continue
			}
			m.cursor = index
			if wasExpanded {
				m.expanded = skill.Name
			}
			break
		}
		if message.result.Disabled {
			m.status = "disabled model invocation for " + message.result.Skill
		} else {
			m.status = "enabled model invocation for " + message.result.Skill
		}
		m.syncViewport()
	case editDone:
		m.applySourceEditDone(message)
	case enabledEditDone:
		m.applyEnabledEditDone(message)
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
		return updateMouse(m, message)
	case tea.KeyMsg:
		return updateKey(m, message)
	}
	if m.quitAfterBusy && !m.busy {
		return m, tea.Quit
	}
	return m, nil
}

func (m *model) applySourceEditDone(message editDone) {
	m.busy = false
	if message.editorErr != nil {
		m.status = "editor failed: " + message.editorErr.Error()
		return
	}
	if message.refreshErr != nil {
		m.status = "refresh failed: " + message.refreshErr.Error()
		return
	}
	wasExpanded := m.expanded == message.skill
	m.replaceManagedSkills(message.skills)
	m.applyCatalog()
	m.applySelectionState(message.selection)
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
}

func (m *model) applyEnabledEditDone(message enabledEditDone) {
	m.busy = false
	if message.editorErr != nil {
		m.status = "editor failed: " + message.editorErr.Error()
		return
	}
	if message.refreshErr != nil {
		m.status = "refresh failed: " + message.refreshErr.Error()
		return
	}
	m.applySelectionState(message.selection)
	m.remoteSelected = remoteSelectionFrom(m.allSkills, m.selected)
	for index, skillIndex := range m.localSkillIndices() {
		if m.skills[skillIndex].Name == message.skill {
			m.cursor = index
			break
		}
	}
	m.syncViewport()
	m.status = "saved enabled value for " + message.skill
}

func (m *model) applySelectionState(selection selectionState) {
	m.selected = selection.selected
	m.globalSelected = selection.globalSelected
	m.projectSelected = selection.projectSelected
	m.globalConditional = selection.globalExpressions
	m.projectConditional = selection.projectExpressions
}

func (m *model) replaceManagedSkills(skills []discoveredSkill) {
	for _, skill := range m.allSkills {
		if isNativeSkill(skill) {
			skills = append(skills, skill)
		}
	}
	slices.SortFunc(skills, compareDiscoveredSkills)
	m.allSkills = skills
}

func updateMouse(m model, message tea.MouseMsg) (tea.Model, tea.Cmd) {
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
	header := m.headerHeight()
	if len(sourceSubtabsFor(m.tab)) > 0 && mouse.Y == header-2 {
		if subtab, ok := m.sourceSubtabAt(mouse.X); ok {
			return m, m.selectSourceSubtab(subtab)
		}
		return m, nil
	}
	if mouse.Y == header-1 {
		m.startFiltering()
		return m, nil
	}
	if m.filtering {
		return m, nil
	}
	index, ok := m.itemIndexAtHeaderRow(mouse.Y)
	if !ok {
		return m, nil
	}
	m.toggleCurrentExpanded(index)
	return m, nil
}

func updateKey(m model, message tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.busy {
		if message.String() == "ctrl+c" {
			if m.busyCancel != nil {
				m.busyCancel()
				m.quitAfterBusy = true
				m.status = "canceling current operation"
				return m, nil
			}
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
	case "[":
		if len(sourceSubtabsFor(m.tab)) > 0 {
			return m, m.selectSourceSubtab(m.wrapSourceSubtab(m.sourceSubtabs[m.tab] - 1))
		}
	case "]":
		if len(sourceSubtabsFor(m.tab)) > 0 {
			return m, m.selectSourceSubtab(m.wrapSourceSubtab(m.sourceSubtabs[m.tab] + 1))
		}
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
		m.toggleCurrentExpanded(m.cursor)
	case " ":
		return toggleSelectedSkill(m)
	case "m":
		if !isLocalTab(m.tab) || m.manager == nil {
			break
		}
		skillIndex, ok := m.localSkillIndex(m.cursor)
		if !ok {
			break
		}
		skill := m.skills[skillIndex]
		if isNativeSkill(skill) {
			m.status = externalSkillControlStatus(skill)
			break
		}
		m.busy = true
		m.status = "updating model invocation for " + skill.Name
		ctx, cancel := context.WithCancel(context.Background())
		m.busyCancel = cancel
		return m, func() tea.Msg {
			result, err := m.manager.toggleModelInvocation(
				ctx,
				m.project,
				skill,
			)
			return modelInvocationDone{result: result, err: err}
		}
	case "a":
		return relocateSelectedSkill(m)
	case "u":
		if !isLocalTab(m.tab) || m.manager == nil {
			break
		}
		skillIndex, ok := m.localSkillIndex(m.cursor)
		if !ok {
			break
		}
		skill := m.skills[skillIndex]
		if skill.RemoteKey == "" {
			break
		}
		m.busy = true
		m.status = "uninstalling " + skill.Name
		ctx, cancel := context.WithCancel(context.Background())
		m.busyCancel = cancel
		return m, func() tea.Msg {
			result, err := m.manager.uninstallRemote(
				ctx,
				m.project,
				skill.Name,
				skill.RemoteKey,
			)
			return remoteUninstallDone{result: result, err: err}
		}
	case "i":
		if !isLocalTab(m.tab) {
			break
		}
		skillIndex, ok := m.localSkillIndex(m.cursor)
		if !ok {
			break
		}
		skill := m.skills[skillIndex]
		if isNativeSkill(skill) {
			m.status = externalSkillControlStatus(skill)
			break
		}
		command, draft, err := m.enabledEditor(skill.Name)
		if err != nil {
			m.status = "error: " + err.Error()
			break
		}
		m.busy = true
		m.status = "editing enabled value for " + skill.Name
		return m, tea.ExecProcess(command, func(err error) tea.Msg {
			if err != nil {
				_ = os.Remove(draft)
				return enabledEditDone{skill: skill.Name, editorErr: err}
			}
			selection, refreshErr := m.manager.applyEnabledDraft(
				m.project,
				skill,
				draft,
			)
			return enabledEditDone{
				skill:      skill.Name,
				selection:  selection,
				refreshErr: refreshErr,
			}
		})
	case "e":
		if !isLocalTab(m.tab) {
			break
		}
		skillIndex, ok := m.localSkillIndex(m.cursor)
		if !ok {
			break
		}
		skill := m.skills[skillIndex]
		if isNativeSkill(skill) {
			m.status = externalSkillControlStatus(skill)
			break
		}
		edit, err := m.editor(skill.Name)
		if err != nil {
			m.status = "error: " + err.Error()
			break
		}
		m.busy = true
		m.status = "editing " + skill.Name
		return m, tea.ExecProcess(edit.command, func(err error) tea.Msg {
			if edit.draft != "" {
				defer os.Remove(edit.draft)
			}
			if err != nil {
				return editDone{skill: skill.Name, editorErr: err}
			}
			skills, selection, refreshErr := m.manager.completeSkillEdit(
				context.Background(),
				m.project,
				edit.source,
				edit.draft,
				edit.layeredDigest,
			)
			return editDone{
				skill: skill.Name, path: edit.source.Path,
				skills: skills, selection: selection,
				refreshErr: refreshErr,
			}
		})
	}
	return m, nil
}

func (m *manager) completeSkillEdit(
	ctx context.Context,
	project string,
	edited discoveredSkill,
	draft string,
	layeredDigest [sha256.Size]byte,
) ([]discoveredSkill, selectionState, error) {
	if edited.RemoteKey == "" {
		return m.refreshEditedSkill(project, edited.Name, edited.Path)
	}
	if draft == "" {
		return nil, selectionState{}, fmt.Errorf("remote skill edit draft is missing")
	}
	contents, err := os.ReadFile(draft)
	if err != nil {
		return nil, selectionState{}, fmt.Errorf("read remote skill edit: %w", err)
	}
	ref, err := m.persistedRemoteRef(edited.RemoteKey, edited.Name)
	if err != nil {
		return nil, selectionState{}, err
	}
	if err := m.remoteStore.savePatch(
		ctx,
		ref,
		edited.Path,
		layeredDigest,
		contents,
	); err != nil {
		return nil, selectionState{}, err
	}
	skills, err := m.skills(project)
	if err != nil {
		return nil, selectionState{}, err
	}
	selection, err := m.selectionState(project, skills)
	return skills, selection, err
}

// relocateSelectedSkill moves the highlighted skill between $HOME/.agents/skills
// and the manager home. Only those two roots take part: content in a
// harness-owned root belongs to that harness, and a project skill is committed
// where every harness can see it on purpose.
func relocateSelectedSkill(m model) (tea.Model, tea.Cmd) {
	if m.tab != localTab || m.manager == nil {
		return m, nil
	}
	skillIndex, ok := m.localSkillIndex(m.cursor)
	if !ok {
		return m, nil
	}
	skill := m.skills[skillIndex]
	if skill.Source != "user" && skill.Source != managedSkillSource {
		m.status = "only a user or managed skill can be adopted or released"
		return m, nil
	}
	action := "adopting "
	if skill.Source == managedSkillSource {
		action = "releasing "
	}
	m.busy = true
	m.status = action + skill.Name
	return m, func() tea.Msg {
		result, err := m.manager.relocateSkill(m.project, skill)
		return skillLocationDone{result: result, err: err}
	}
}

func toggleSelectedSkill(m model) (tea.Model, tea.Cmd) {
	if !isLocalTab(m.tab) {
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
	skill := m.skills[skillIndex]
	if skill.Source == claudePluginSource {
		m.status = "Claude plugin status is display only"
		return m, nil
	}
	if isGrokNativeSkill(skill) {
		m.busy = true
		m.status = "updating " + skill.Name + " in Grok"
		return m, func() tea.Msg {
			enabled, err := m.manager.toggleGrokSkill(skill.Name)
			return grokToggleDone{skill: skill.Name, enabled: enabled, err: err}
		}
	}
	m.busy = true
	m.status = "updating " + skill.Name
	return m, func() tea.Msg {
		enabled, err := m.manager.toggle(m.project, skill.Name, skill.RemoteKey)

		return toggleDone{skill: skill.Name, enabled: enabled, err: err}
	}
}

func isGrokNativeSkill(skill discoveredSkill) bool {
	return skill.Source == grokPluginSource || skill.Source == grokBundledSource
}

func isNativeSkill(skill discoveredSkill) bool {
	return skill.Source == claudePluginSource || isGrokNativeSkill(skill)
}

func externalSkillControlStatus(skill discoveredSkill) string {
	if skill.Source == claudePluginSource {
		return "Claude plugin skills are display only"
	}
	return "Grok owns this skill; use Space to change its global status"
}

func (m model) wrapSourceSubtab(subtab int) int {
	count := len(sourceSubtabsFor(m.tab))
	return (subtab%count + count) % count
}

func (m *model) selectSourceSubtab(subtab int) tea.Cmd {
	subtabs := sourceSubtabsFor(m.tab)
	if subtab < 0 || subtab >= len(subtabs) || subtab == m.sourceSubtabs[m.tab] {
		return nil
	}
	m.sourceSubtabs[m.tab] = subtab
	m.cursor = 0
	m.offset = 0
	m.expanded = ""
	m.applyCatalog()
	m.status = fmt.Sprintf("%d skills", len(m.skills))
	m.syncViewport()
	return nil
}

func (m *model) selectTab(tab int) tea.Cmd {
	if tab < localTab || tab > skillsMPTab || m.tab == tab {
		return nil
	}
	m.cancelRegistryRequest()
	m.tab = tab
	m.cursor = 0
	m.offset = 0
	m.expanded = ""
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
		m.applyCatalog()
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
	switch {
	case m.remoteError != "":
		m.status = "error: " + m.remoteError
	case len(m.remoteTopics) == 0:
		m.status = "remote cache is empty; run skills-mgr daemon"
	default:
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
	return m.currentList().count
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
) ([]discoveredSkill, selectionState, error) {
	skills, err := m.skills(project)
	if err != nil {
		return nil, selectionState{}, err
	}
	var edited discoveredSkill
	for _, skill := range skills {
		if skill.Path == path {
			edited = skill
			break
		}
	}
	newName := edited.Name
	var undoSelection func() error
	if newName != "" && newName != oldName {
		cloneLock := func(value lock) lock {
			value.Skills = maps.Clone(value.Skills)
			value.Expressions = maps.Clone(value.Expressions)
			value.Remote = maps.Clone(value.Remote)
			return value
		}
		sameLock := func(left, right lock) bool {
			return maps.Equal(left.Skills, right.Skills) &&
				maps.Equal(left.Expressions, right.Expressions) &&
				maps.Equal(left.Remote, right.Remote)
		}
		mergeEnabled := func(oldEnabled, newEnabled enabledValue) (enabledValue, error) {
			if oldEnabled.Boolean == nil || newEnabled.Boolean == nil {
				return enabledValue{}, fmt.Errorf(
					"cannot merge enabled values while renaming skill %q to %q when either value is an expression",
					oldName,
					newName,
				)
			}
			merged := *newEnabled.Boolean || *oldEnabled.Boolean
			return enabledValue{Boolean: new(merged)}, nil
		}

		var previousLock, renamedLock lock
		renamedSelection := false
		err = m.updateSelectionLock(project, func(value *lock) (bool, error) {
			previousLock = cloneLock(*value)
			oldEnabled, exists := value.enabled(oldName)
			if m.global {
				if !exists {
					return false, nil
				}
				newEnabled, newExists := value.enabled(newName)
				switch {
				case !newExists:
					value.setEnabled(newName, oldEnabled)
				default:
					merged, err := mergeEnabled(oldEnabled, newEnabled)
					if err != nil {
						return false, err
					}
					value.setEnabled(newName, merged)
				}
				value.deleteEnabled(oldName)
				renamedLock = cloneLock(*value)
				renamedSelection = true
				return true, nil
			}

			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return false, err
			}
			effective := oldEnabled
			effectiveExists := exists
			if !effectiveExists {
				effective, effectiveExists = global.enabled(oldName)
			}
			if !effectiveExists ||
				(!exists && effective.Boolean != nil && !*effective.Boolean) {
				return false, nil
			}
			newEnabled, newExists := value.enabled(newName)
			switch {
			case !newExists:
				value.setEnabled(newName, effective)
			default:
				merged, err := mergeEnabled(effective, newEnabled)
				if err != nil {
					return false, err
				}
				value.setEnabled(newName, merged)
			}
			if _, inherited := global.enabled(oldName); inherited {
				value.setEnabled(oldName, enabledValue{Boolean: new(false)})
			} else {
				value.deleteEnabled(oldName)
			}
			renamedLock = cloneLock(*value)
			renamedSelection = true
			return true, nil
		})
		if err != nil {
			return nil, selectionState{}, err
		}
		if renamedSelection {
			undoSelection = func() error {
				return m.updateSelectionLock(project, func(value *lock) (bool, error) {
					if !sameLock(*value, renamedLock) {
						return false, fmt.Errorf("selection changed during managed edit rollback")
					}
					*value = cloneLock(previousLock)
					return true, nil
				})
			}
		}
	}
	rollbackSelection := func(cause error) error {
		if undoSelection == nil {
			return cause
		}
		return errors.Join(cause, undoSelection())
	}
	selection, err := m.selectionState(project, skills)
	if err != nil || edited.Source != managedSkillSource {
		if err != nil {
			err = rollbackSelection(err)
		}
		return skills, selection, err
	}
	frontmatter, err := m.placeholderFrontmatter(edited, nil)
	if err != nil {
		return skills, selection, rollbackSelection(err)
	}
	var changes []placeholderChange
	if m.global {
		global, err := loadLock(m.paths.globalLockDir)
		if err != nil {
			return skills, selection, rollbackSelection(err)
		}
		if oldName != newName {
			changes = append(changes, placeholderChange{base: m.paths.placeholderDir, name: oldName})
		}
		changes = append(changes, placeholderChange{
			base:    m.paths.placeholderDir,
			name:    newName,
			enabled: lockWantsPlaceholder(global, newName),
		})
	} else {
		projectLock, err := loadLock(project)
		if err != nil {
			return skills, selection, rollbackSelection(err)
		}
		sameRoot := filepath.Clean(project) == filepath.Clean(m.paths.placeholderDir)
		if sameRoot || oldName == newName {
			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return skills, selection, rollbackSelection(err)
			}
			if sameRoot && oldName != newName {
				changes = append(changes,
					placeholderChange{
						base: project,
						name: oldName,
						enabled: lockWantsPlaceholder(global, oldName) ||
							lockWantsPlaceholder(projectLock, oldName),
					},
					placeholderChange{
						base: project,
						name: newName,
						enabled: lockWantsPlaceholder(global, newName) ||
							lockWantsPlaceholder(projectLock, newName),
					},
				)
			} else {
				enabled := lockWantsPlaceholder(global, newName)
				if sameRoot {
					enabled = enabled || lockWantsPlaceholder(projectLock, newName)
				}
				changes = append(changes, placeholderChange{
					base:    m.paths.placeholderDir,
					name:    newName,
					enabled: enabled,
				})
			}
		}
		if !sameRoot {
			if oldName != newName {
				changes = append(changes, placeholderChange{base: project, name: oldName})
			}
			changes = append(changes, placeholderChange{
				base:    project,
				name:    newName,
				enabled: lockWantsPlaceholder(projectLock, newName),
			})
		}
	}
	_, err = m.changeRemotePlaceholdersAcross(changes, frontmatter)
	if err != nil {
		err = rollbackSelection(err)
	}
	return skills, selection, err
}

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

func (m *model) syncViewport() renderedList {
	list := m.currentList()
	if list.count == 0 {
		m.cursor = 0
		m.offset = 0
		return list
	}
	m.cursor = min(max(m.cursor, 0), list.count-1)
	m.offset = min(max(m.offset, 0), list.count-1)
	list = m.currentList()
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
			if effectiveGlobalConditional && (m.manager == nil || !m.manager.global) {
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
			styledPathLines(terminalSafeText(skill.Path), m.width)...,
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
		color = lipgloss.AdaptiveColor{Light: "#6F42C1", Dark: "#C4A7E7"}
	case skill.Source == "user":
		color = lipgloss.AdaptiveColor{Light: "#005F87", Dark: "#7DD3FC"}
	case skill.Source == managedSkillSource:
		color = lipgloss.AdaptiveColor{Light: "#237A3B", Dark: "#75D18B"}
	case skill.Source == "plugin", skill.Source == claudePluginSource, skill.Source == grokPluginSource:
		color = lipgloss.AdaptiveColor{Light: "#8A5A00", Dark: "#E5C07B"}
	case skill.Source == "bundled", skill.Source == grokBundledSource:
		color = lipgloss.AdaptiveColor{Light: "#A63D73", Dark: "#F49AC2"}
	}
	return lipgloss.NewStyle().Foreground(color)
}

func (m model) localSkillIndices() []int {
	indices := make([]int, 0, len(m.skills))
	query := strings.ToLower(m.filterQuery)
	for group := range 4 {
		for index, skill := range m.skills {
			if m.localSkillGroup(skill) != group {
				continue
			}
			if matchesNormalizedFilter(
				query,
				skill.Name,
				skill.Description,
				skill.Path,
				skill.Source,
				skill.Plugin,
				skill.Vendor,
				skill.CompatibilityStatus,
			) {
				indices = append(indices, index)
			}
		}
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

func styledPathLines(path string, width int) []string {
	return styledMetadataLines("path", path, width)
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

func configuredEditor(file string) (*exec.Cmd, error) {
	parts, err := shellwords.Parse(os.Getenv("EDITOR"))
	if err != nil || len(parts) == 0 {
		return nil, fmt.Errorf("$EDITOR is not set or invalid")
	}
	return exec.Command(parts[0], append(parts[1:], file)...), nil
}

func (m model) editor(skill string) (_ skillEdit, retErr error) {
	discovered, err := m.manager.findSkill(m.project, skill)
	if err != nil {
		return skillEdit{}, err
	}
	if !discovered.Editable {
		return skillEdit{}, fmt.Errorf("skill %q is not editable at its discovered source", skill)
	}
	file := discovered.Path
	if info, err := os.Stat(file); err != nil || !info.Mode().IsRegular() {
		return skillEdit{}, fmt.Errorf("missing %s", file)
	}
	edit := skillEdit{source: discovered}
	if discovered.RemoteKey != "" {
		original, err := os.ReadFile(file)
		if err != nil {
			return skillEdit{}, fmt.Errorf("read remote skill: %w", err)
		}
		ref, err := m.manager.persistedRemoteRef(discovered.RemoteKey, discovered.Name)
		if err != nil {
			return skillEdit{}, err
		}
		contents, err := m.manager.remoteStore.applyPatch(ref, original)
		if err != nil {
			if errors.Is(err, errRemoteSkillPatch) {
				// Fall back to original content when patch no longer applies
				contents = original
			} else {
				return skillEdit{}, err
			}
		}
		temporary, err := os.CreateTemp("", "skills-mgr-remote-skill-*.md")
		if err != nil {
			return skillEdit{}, fmt.Errorf("create remote skill edit: %w", err)
		}
		edit.draft = temporary.Name()
		edit.layeredDigest = sha256.Sum256(contents)
		defer func() {
			if retErr != nil {
				_ = temporary.Close()
				_ = os.Remove(edit.draft)
			}
		}()
		if err := temporary.Chmod(0o600); err != nil {
			return skillEdit{}, err
		}
		if _, err := temporary.Write(contents); err != nil {
			return skillEdit{}, fmt.Errorf("write remote skill edit: %w", err)
		}
		if err := temporary.Close(); err != nil {
			return skillEdit{}, fmt.Errorf("close remote skill edit: %w", err)
		}
		file = edit.draft
	}
	edit.command, err = configuredEditor(file)
	return edit, err
}

func (m model) enabledEditor(skill string) (_ *exec.Cmd, draft string, retErr error) {
	value, err := loadLock(m.manager.lockDir(m.project))
	if err != nil {
		return nil, "", err
	}
	current, configured := value.enabled(skill)
	temp, err := os.CreateTemp("", "skills-mgr-enabled-*.json")
	if err != nil {
		return nil, "", fmt.Errorf("create enabled editor draft: %w", err)
	}
	draft = temp.Name()
	draftPath := draft
	defer func() {
		if retErr != nil {
			_ = temp.Close()
			_ = os.Remove(draftPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return nil, "", fmt.Errorf("secure enabled editor draft: %w", err)
	}
	if configured {
		var data []byte
		if current.Boolean != nil {
			data = fmt.Appendf(data, "%t", *current.Boolean)
		} else {
			data = append(data, current.Expression...)
		}
		data = append(data, '\n')
		if _, err := temp.Write(data); err != nil {
			return nil, "", fmt.Errorf("write enabled editor draft: %w", err)
		}
	}
	if err := temp.Close(); err != nil {
		return nil, "", fmt.Errorf("close enabled editor draft: %w", err)
	}
	command, err := configuredEditor(draft)
	if err != nil {
		return nil, "", err
	}
	return command, draft, nil
}

func (m *manager) applyEnabledDraft(
	project string,
	skill discoveredSkill,
	draft string,
) (selectionState, error) {
	defer os.Remove(draft)
	data, err := os.ReadFile(draft)
	if err != nil {
		return selectionState{}, fmt.Errorf("read enabled editor draft: %w", err)
	}
	var desired enabledValue
	draftValue := strings.TrimSpace(string(data))
	desiredExists := draftValue != ""
	if desiredExists {
		switch {
		case draftValue == "true" || draftValue == "false" || strings.HasPrefix(draftValue, `"`):
			if err := json.Unmarshal([]byte(draftValue), &desired); err != nil {
				return selectionState{}, fmt.Errorf("decode enabled editor draft: %w", err)
			}
		default:
			desired.Expression = draftValue
		}
	}

	var undoPlaceholder func() error
	if placeholderManaged(skill) {
		var ref *remoteSkillRef
		if skill.RemoteKey != "" {
			resolved, err := m.persistedRemoteRef(skill.RemoteKey, skill.Name)
			if err != nil {
				return selectionState{}, err
			}
			ref = &resolved
		}
		frontmatter, err := m.placeholderFrontmatter(skill, ref)
		if err != nil {
			return selectionState{}, err
		}
		effective, effectiveExists := desired, desiredExists
		if !desiredExists && !m.global {
			global, err := loadLock(m.paths.globalLockDir)
			if err != nil {
				return selectionState{}, err
			}
			effective, effectiveExists = global.enabled(skill.Name)
		}
		// A conditional entry still gets a placeholder. The stub is invisible to
		// the model, so it cannot bypass a false condition; only list, get, and
		// run answer for the model, and all three evaluate the expression.
		create := effectiveExists &&
			(effective.Boolean == nil || *effective.Boolean)
		undoPlaceholder, err = m.changeRemotePlaceholders(
			project,
			skill.Name,
			frontmatter,
			create,
		)
		if err != nil {
			return selectionState{}, err
		}
	}

	err = m.updateSelectionLock(project, func(value *lock) (bool, error) {
		current, currentExists := value.enabled(skill.Name)
		same := currentExists == desiredExists
		if same && currentExists {
			same = current.Expression == desired.Expression &&
				((current.Boolean == nil && desired.Boolean == nil) ||
					(current.Boolean != nil &&
						desired.Boolean != nil &&
						*current.Boolean == *desired.Boolean))
		}
		if same {
			return false, nil
		}
		if desiredExists {
			value.setEnabled(skill.Name, desired)
		} else {
			value.deleteEnabled(skill.Name)
		}
		return true, nil
	})
	if err != nil {
		if undoPlaceholder != nil {
			err = errors.Join(err, undoPlaceholder())
		}
		return selectionState{}, err
	}
	return m.selectionState(project, nil)
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
