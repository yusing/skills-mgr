package main

import (
	"context"

	"fmt"

	"time"

	tea "github.com/charmbracelet/bubbletea"
)

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
