package main

import (
	"context"

	"os"

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
			return m, nil
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
