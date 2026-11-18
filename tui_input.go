package main

import (
	tea "github.com/charmbracelet/bubbletea"
)

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
			m.selectSourceSubtab(subtab)
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
	key := message.String()
	if m.busy {
		if key == "ctrl+c" {
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
		switch key {
		case "ctrl+c":
			m.cancelRegistryRequest()
			return m, tea.Quit
		case "enter", "esc":
			m.filtering = false
		case "backspace":
			if runes := []rune(m.filterQuery); len(runes) > 0 {
				m.filterQuery = string(runes[:len(runes)-1])
				cmd := m.refreshList()
				return m, cmd
			}
		default:
			if message.Type == tea.KeyRunes {
				m.filterQuery += string(message.Runes)
				cmd := m.refreshList()
				return m, cmd
			}
		}
		return m, nil
	}
	switch key {
	case "q", "ctrl+c":
		m.cancelRegistryRequest()
		return m, tea.Quit
	case "f":
		m.startFiltering()
	case "left":
		cmd := m.selectTab(max(localTab, m.tab-1))
		return m, cmd
	case "right":
		cmd := m.selectTab(min(skillsMPTab, m.tab+1))
		return m, cmd
	case "[":
		if len(sourceSubtabsFor(m.tab)) > 0 {
			m.selectSourceSubtab(m.wrapSourceSubtab(m.sourceSubtabs[m.tab] - 1))
			return m, nil
		}
	case "]":
		if len(sourceSubtabsFor(m.tab)) > 0 {
			m.selectSourceSubtab(m.wrapSourceSubtab(m.sourceSubtabs[m.tab] + 1))
			return m, nil
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
	case " ", "m", "a", "u", "i", "e":
		cmd := m.skillAction(key)
		return m, cmd
	}
	return m, nil
}

func (m *model) skillAction(key string) tea.Cmd {
	if !isLocalTab(m.tab) {
		if key == " " {
			return m.toggleRegistrySkill()
		}
		return nil
	}
	index, ok := m.localSkillIndex(m.cursor)
	if !ok {
		return nil
	}
	skill := m.skills[index]
	if skill.ContentMissing && key != "u" {
		m.status = "content missing: use sync or reinstall, or u to uninstall"
		return nil
	}
	switch key {
	case " ":
		return m.toggleLocalSkill(skill)
	case "a":
		return m.relocateSelectedSkill(skill)
	case "u":
		if m.manager == nil || skill.RemoteKey == "" {
			return nil
		}
		ctx := m.startCancelableJob("uninstalling " + skill.Name)
		return func() tea.Msg {
			snapshot, err := m.manager.tuiUninstall(ctx, m.project, skill.Name, skill.RemoteKey)
			return remoteUninstallDone{skill: skill.Name, snapshot: snapshot, err: err}
		}
	case "m", "i", "e":
		if key == "m" && m.manager == nil {
			return nil
		}
		if isNativeSkill(skill) {
			m.status = externalSkillControlStatus(skill)
			return nil
		}
		switch key {
		case "i":
			return m.editEnabledValue(skill)
		case "e":
			return m.editSkill(skill.Name)
		}
		ctx := m.startCancelableJob("updating model invocation for " + skill.Name)
		return func() tea.Msg {
			result, err := m.manager.toggleModelInvocation(ctx, m.project, skill)
			return modelInvocationDone{result: result, remoteKey: skill.RemoteKey, err: err}
		}
	}
	return nil
}

// relocateSelectedSkill moves the highlighted skill between $HOME/.agents/skills
// and the manager home. Only those two roots take part: content in a
// harness-owned root belongs to that harness, and a project skill is committed
// where every harness can see it on purpose.
func (m *model) relocateSelectedSkill(skill discoveredSkill) tea.Cmd {
	if m.tab != localTab || m.manager == nil {
		return nil
	}
	if skill.Source != "user" && skill.Source != managedSkillSource {
		m.status = "only a user or managed skill can be adopted or released"
		return nil
	}
	action := "adopting "
	if skill.Source == managedSkillSource {
		action = "releasing "
	}
	m.busy = true
	m.status = action + skill.Name
	return func() tea.Msg {
		result, err := m.manager.relocateSkill(m.project, skill)
		return skillLocationDone{result: result, err: err}
	}
}

func (m *model) toggleRegistrySkill() tea.Cmd {
	ref, ok := remoteRefAtCursor(*m)
	if !ok || m.manager == nil {
		return nil
	}
	status := "disabling "
	if !m.remoteSelected[ref.key()] {
		status = "installing "
		m.progressTitle = "Installing " + ref.Name
		m.progressDetail = "Checking cached content and downloading if needed…"
	}
	ctx := m.startCancelableJob(status + ref.Name)
	return func() tea.Msg {
		result, err := m.manager.tuiToggleRemote(ctx, m.project, ref)
		return remoteToggleDone{skill: ref.Name, result: result, err: err}
	}
}

func (m *model) toggleLocalSkill(skill discoveredSkill) tea.Cmd {
	if skill.Source == claudePluginSource {
		m.status = "Claude plugin status is display only"
		return nil
	}
	if isGrokNativeSkill(skill) {
		ctx := m.startCancelableJob("updating " + skill.Name + " in Grok")
		return func() tea.Msg {
			enabled, err := m.manager.tuiToggleGrok(ctx, m.project, skill.Name)
			return grokToggleDone{skill: skill.Name, enabled: enabled, err: err}
		}
	}
	m.busy = true
	m.status = "updating " + skill.Name
	return func() tea.Msg {
		enabled, err := m.manager.tuiToggle(m.project, skill)
		return toggleDone{skill: skill.Name, enabled: enabled, err: err}
	}
}
