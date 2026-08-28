package main

import (
	"context"
	"crypto/sha256"

	"fmt"
	"os"
	"os/exec"

	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
			m.remoteSelected = remoteSelectionFrom(m.allSkills, m.selected)
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
		m.applyCatalogResult(
			message.result.Skills,
			message.result.Selected,
			message.result.GlobalSelected,
			message.result.ProjectSelected,
		)
		if m.projectSelected != nil {
			delete(m.projectConditional, message.result.Skill)
		} else {
			delete(m.globalConditional, message.result.Skill)
		}
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
		m.applyCatalogResult(
			message.result.Skills,
			message.result.Selected,
			message.result.GlobalSelected,
			message.result.ProjectSelected,
		)
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
		m.applyCatalogResult(
			message.result.Skills,
			message.result.Selected,
			message.result.GlobalSelected,
			message.result.ProjectSelected,
		)
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

// applyCatalogResult adopts a refreshed catalog together with the selection maps
// that were computed against it, so every action that rebuilds the catalog keeps
// the same derived model fields in step.
func (m *model) applyCatalogResult(
	skills []discoveredSkill,
	selected, global, project map[string]bool,
) {
	m.replaceManagedSkills(skills)
	m.applyCatalog()
	m.selected = selected
	m.globalSelected = global
	m.projectSelected = project
	m.remoteSelected = remoteSelectionFrom(m.allSkills, m.selected)
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
