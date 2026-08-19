package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestSkillListScrollsToKeepCursorVisible(t *testing.T) {
	current := model{
		skills:   skillNames(8),
		selected: map[string]bool{"skill-04": true},
	}
	updated, _ := current.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	current = updated.(model)
	for range 5 {
		updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyDown})
		current = updated.(model)
	}

	if current.cursor != 5 || current.offset != 3 {
		t.Fatalf("cursor, offset = %d, %d; want 5, 3", current.cursor, current.offset)
	}
	view := current.View()
	for _, want := range []string{"  ▸ skill-02 [disabled]", "> ▸ skill-05 [disabled]"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view does not contain %q:\n%s", want, view)
		}
	}
	for _, hidden := range []string{"skill-00", "skill-01", "skill-04", "skill-06"} {
		if strings.Contains(view, hidden) {
			t.Fatalf("view contains skill outside viewport %q:\n%s", hidden, view)
		}
	}
}

func TestRenderedListCachesItemLines(t *testing.T) {
	renders := 0
	list := newRenderedList(1, func(int) []string {
		renders++
		return []string{"header", "detail"}
	})

	if got := len(list.lines(0)); got != 2 {
		t.Fatalf("line count = %d, want 2", got)
	}
	if got := len(list.lines(0)); got != 2 {
		t.Fatalf("cached line count = %d, want 2", got)
	}
	if renders != 1 {
		t.Fatalf("render callback called %d times, want 1", renders)
	}

	nilRenders := 0
	empty := newRenderedList(1, func(int) []string {
		nilRenders++
		return nil
	})
	empty.lines(0)
	empty.lines(0)
	if nilRenders != 1 {
		t.Fatalf("nil render callback called %d times, want 1", nilRenders)
	}
}

func TestSkillListStopsAtNavigationBoundaries(t *testing.T) {
	current := model{skills: skillNames(3), selected: map[string]bool{}}
	updated, _ := current.Update(tea.WindowSizeMsg{Width: 80, Height: 8})
	current = updated.(model)

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyUp})
	current = updated.(model)
	if current.cursor != 0 || current.offset != 0 {
		t.Fatalf("up at start moved cursor, offset to %d, %d", current.cursor, current.offset)
	}

	for range 4 {
		updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyDown})
		current = updated.(model)
	}
	if current.cursor != 2 || current.offset != 2 {
		t.Fatalf("down past end moved cursor, offset to %d, %d; want 2, 2", current.cursor, current.offset)
	}
}

func TestTabsNavigateWithArrowKeys(t *testing.T) {
	skills := skillNames(2)
	current := model{
		skills:    skills,
		allSkills: skills,
		selected:  map[string]bool{},
		remoteTopics: []remoteTopic{{
			Slug: "testing", Name: "Testing",
			Skills: []remoteSkill{{ID: "owner/repo/alpha", Name: "alpha"}},
		}},
		width:  80,
		height: 10,
	}
	updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyRight})
	current = updated.(model)
	if current.tab != codexTab || current.cursor != 0 {
		t.Fatalf("right selected tab, cursor = %d, %d; want %d, 0", current.tab, current.cursor, codexTab)
	}
	if view := current.View(); !strings.Contains(view, "[Codex]") {
		t.Fatalf("codex tab omitted label:\n%s", view)
	}

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyLeft})
	current = updated.(model)
	if current.tab != localTab {
		t.Fatalf("left selected tab = %d, want %d", current.tab, localTab)
	}
	if view := current.View(); !strings.Contains(view, "[Installed]") ||
		!strings.Contains(view, "skill-00") {
		t.Fatalf("installed tab omitted local skills:\n%s", view)
	}

	current.tab = claudeTab
	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyRight})
	current = updated.(model)
	if current.tab != remoteTab {
		t.Fatalf("right from Claude selected tab = %d, want %d", current.tab, remoteTab)
	}
	if view := current.View(); !strings.Contains(view, "[skills.sh]") ||
		!strings.Contains(view, "Testing") || !strings.Contains(view, "alpha") {
		t.Fatalf("remote tab omitted topic tree:\n%s", view)
	}
}

func TestSkillsMPTabLoadsDefaultCache(t *testing.T) {
	manager := newTestManager(t)
	manager.paths.skillsMP = filepath.Join(t.TempDir(), "skillsmp.json")
	manager.skillsMP = newSkillsMPRegistry(manager.paths.skillsMP, "")
	if err := saveSkillsMPCache(manager.paths.skillsMP, skillsMPCache{
		SchemaRevision: skillsMPSchemaRevision,
		UpdatedAt:      time.Now(),
		Skills: []skillsMPSkill{
			{ID: "alpha-id", Name: "alpha", Author: "owner", Stars: 42},
			{
				ID: "localized-alpha-id", Name: "alpha", Author: "owner",
				Stars: 42,
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	current := model{
		manager: manager, skills: skillNames(1), selected: map[string]bool{},
		width: 60, height: 10,
	}
	current.tab = remoteTab
	updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyRight})
	current = updated.(model)

	view := current.View()
	if current.tab != skillsMPTab || current.itemCount() != 1 ||
		!strings.Contains(view, "[SkillsMP]") ||
		!strings.Contains(view, "alpha") ||
		!strings.Contains(view, "owner • 42 stars") {
		t.Fatalf("SkillsMP tab omitted cached catalog:\n%s", view)
	}
}

func TestSkillsMPTabFetchesAndCachesDefaultCatalogOnDemand(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/api/skills" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"skills": []map[string]any{{
				"id": "fresh-id", "name": "fresh", "author": "remote", "stars": 12,
			}},
		})
	}))
	defer server.Close()
	manager := newTestManager(t)
	manager.paths.skillsMP = filepath.Join(t.TempDir(), "skillsmp.json")
	manager.skillsMP = newSkillsMPRegistry(manager.paths.skillsMP, "")
	manager.skillsMP.baseURL = server.URL
	manager.skillsMP.client = server.Client()
	current := model{
		manager: manager, skills: skillNames(1), selected: map[string]bool{},
		width: 60, height: 10,
	}

	current.tab = remoteTab
	updated, command := current.Update(tea.KeyMsg{Type: tea.KeyRight})
	current = updated.(model)
	if command == nil || current.status != "loading SkillsMP" {
		t.Fatalf("uncached catalog did not start loading: %#v", current)
	}
	updated, _ = current.Update(command())
	current = updated.(model)
	if requests != 1 || current.itemCount() != 1 ||
		!strings.Contains(current.View(), "fresh") {
		t.Fatalf("on-demand catalog was not rendered:\n%s", current.View())
	}
	cache, err := loadSkillsMPCache(manager.paths.skillsMP)
	if err != nil {
		t.Fatal(err)
	}
	if len(cache.Skills) != 1 || cache.Skills[0].ID != "fresh-id" {
		t.Fatalf("on-demand cache = %#v", cache)
	}
}

func TestSkillsMPFilterUsesSearchAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/skills/search" ||
			request.URL.Query().Get("q") != "beta" {
			t.Errorf("request URL = %s", request.URL.String())
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"skills": []map[string]any{{
					"id": "beta-id", "name": "beta", "author": "remote", "stars": 7,
				}},
			},
		})
	}))
	defer server.Close()
	manager := newTestManager(t)
	manager.skillsMP = newSkillsMPRegistry("", "")
	manager.skillsMP.baseURL = server.URL
	manager.skillsMP.client = server.Client()
	current := model{
		manager: manager, tab: skillsMPTab, filtering: true,
		width: 60, height: 10,
	}

	updated, staleDebounce := current.Update(tea.KeyMsg{
		Type: tea.KeyRunes, Runes: []rune("b"),
	})
	current = updated.(model)
	updated, debounce := current.Update(tea.KeyMsg{
		Type: tea.KeyRunes, Runes: []rune("eta"),
	})
	current = updated.(model)
	if debounce == nil || staleDebounce == nil || current.itemCount() != 0 ||
		current.status != "searching SkillsMP" {
		t.Fatalf("search did not start: %#v", current)
	}
	updated, staleSearch := current.Update(registrySearchRequested{
		request: current.registryRequest,
		tab:     skillsMPTab,
		query:   "b",
	})
	current = updated.(model)
	if staleSearch != nil {
		t.Fatal("stale debounced query started an API request")
	}
	updated, search := current.Update(registrySearchRequested{
		request: current.registryRequest,
		tab:     skillsMPTab,
		query:   "beta",
	})
	current = updated.(model)
	if search == nil {
		t.Fatal("settled debounced query did not start an API request")
	}
	updated, _ = current.Update(search())
	current = updated.(model)
	view := current.View()
	if current.itemCount() != 1 || !strings.Contains(view, "beta") ||
		!strings.Contains(view, "remote • 7 stars") {
		t.Fatalf("search result was not rendered:\n%s", view)
	}
}

func TestRegistrySearchWarningRetainsResults(t *testing.T) {
	current := model{
		tab:             skillsMPTab,
		filterQuery:     "result",
		registryRequest: 4,
		width:           60,
		height:          10,
	}
	updated, _ := current.Update(registrySearchDone{
		request: 4,
		tab:     skillsMPTab,
		query:   "result",
		skills: []registrySearchSkill{{
			ID: "result-id", Name: "result", Label: "owner • 3 stars",
		}},
		err: errors.New("cache write failed"),
	})
	got := updated.(model)
	if len(got.registrySkills) != 1 ||
		got.status != "warning: cache write failed" ||
		!strings.Contains(got.View(), "result") {
		t.Fatalf("partial result warning = %#v\n%s", got, got.View())
	}
}

func TestRegistryTabSwitchClearsPreviousProviderResults(t *testing.T) {
	current := model{
		tab:         remoteTab,
		filterQuery: "active query",
		registrySkills: []registrySearchSkill{{
			ID: "skills-sh-result", Name: "skills.sh result",
		}},
		width:  60,
		height: 10,
	}
	updated, command := current.Update(tea.KeyMsg{Type: tea.KeyRight})
	got := updated.(model)
	if command == nil || got.tab != skillsMPTab ||
		len(got.registrySkills) != 0 ||
		got.status != "searching SkillsMP" ||
		strings.Contains(got.View(), "skills.sh result") {
		t.Fatalf("tab switch retained previous provider state: %#v\n%s", got, got.View())
	}
}

func TestRemoteTopicsExpandAndCollapseAsNestedTree(t *testing.T) {
	current := model{
		tab: remoteTab,
		remoteTopics: []remoteTopic{{
			Slug: "testing", Name: "Testing",
			Skills: []remoteSkill{
				{ID: "owner/repo/alpha", Name: "alpha", Source: "owner/repo", Installs: 42},
				{ID: "owner/repo/beta", Name: "beta", Source: "owner/repo", Installs: 7},
			},
		}},
		width:  60,
		height: 10,
	}
	view := current.View()
	for _, want := range []string{"▾ Testing (2)", "alpha", "owner/repo • 42 installs", "beta"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expanded remote tree omitted %q:\n%s", want, view)
		}
	}

	updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyEnter})
	current = updated.(model)
	view = current.View()
	if !strings.Contains(view, "▸ Testing (2)") || strings.Contains(view, "alpha") {
		t.Fatalf("collapsed remote tree retained children:\n%s", view)
	}
	if current.itemCount() != 1 {
		t.Fatalf("collapsed tree has %d rows, want 1", current.itemCount())
	}
}

func TestRemoteSkillsExpandProviderMetadata(t *testing.T) {
	t.Run("skills.sh topic skill", func(t *testing.T) {
		skill := remoteSkill{
			ID:          "owner/repo/testing",
			Name:        "testing",
			Description: "Runs the repository test suite.",
			Source:      "owner/repo",
			Installs:    42,
		}
		current := model{
			tab: remoteTab,
			remoteTopics: []remoteTopic{{
				Slug: "testing", Name: "Testing", Skills: []remoteSkill{skill},
			}},
			cursor: 1,
			width:  70,
			height: 12,
		}

		updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyEnter})
		current = updated.(model)
		view := current.View()
		for _, want := range []string{
			"▾ testing",
			"Runs the repository test suite.",
			"id  owner/repo/testing",
		} {
			if !strings.Contains(view, want) {
				t.Fatalf("expanded skills.sh skill omitted %q:\n%s", want, view)
			}
		}

		updated, _ = current.Update(tea.MouseMsg{
			X:      4,
			Y:      tuiHeaderHeight + 1,
			Action: tea.MouseActionPress,
			Button: tea.MouseButtonLeft,
		})
		current = updated.(model)
		if current.expanded != "" ||
			strings.Contains(current.View(), "Runs the repository test suite.") {
			t.Fatalf("click did not collapse skills.sh metadata:\n%s", current.View())
		}
	})

	t.Run("SkillsMP skill", func(t *testing.T) {
		current := model{
			tab: skillsMPTab,
			registrySkills: []registrySearchSkill{{
				ID:          "testing-id",
				Name:        "testing",
				Description: "Tests Go packages.",
				Label:       "gopher • 99 stars",
				Provider:    skillsMPProvider,
				Locator:     "https://github.com/owner/repo/tree/main/testing",
			}},
			width:  70,
			height: 12,
		}

		updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyEnter})
		current = updated.(model)
		view := current.View()
		for _, want := range []string{
			"▾ testing",
			"Tests Go packages.",
			"url  https://github.com/owner/repo/tree/main/testing",
		} {
			if !strings.Contains(view, want) {
				t.Fatalf("expanded SkillsMP skill omitted %q:\n%s", want, view)
			}
		}
	})
}

func TestRemoteTabReloadsDaemonCache(t *testing.T) {
	manager := newTestManager(t)
	manager.paths.remoteRegistry = filepath.Join(t.TempDir(), "skills-sh.json")
	if err := saveRemoteCache(manager.paths.remoteRegistry, remoteRegistryCache{
		SchemaRevision: remoteRegistrySchemaRevision,
		Topics: []remoteTopic{{
			Slug: "testing", Name: "Testing",
			Skills: []remoteSkill{{ID: "owner/repo/fresh", Name: "fresh"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	current := model{
		manager:  manager,
		skills:   skillNames(1),
		selected: map[string]bool{},
		width:    60,
		height:   10,
	}
	current.tab = claudeTab
	updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyRight})
	current = updated.(model)
	if len(current.remoteTopics) != 1 ||
		len(current.remoteTopics[0].Skills) != 1 ||
		current.remoteTopics[0].Skills[0].Name != "fresh" {
		t.Fatalf("remote topics = %#v", current.remoteTopics)
	}
	if !strings.Contains(current.View(), "fresh") {
		t.Fatalf("remote view omitted refreshed cache:\n%s", current.View())
	}
}

func TestRemoteTabClearsStaleTopicsWhenCacheReloadFails(t *testing.T) {
	manager := newTestManager(t)
	manager.paths.remoteRegistry = filepath.Join(t.TempDir(), "skills-sh.json")
	writeFile(t, manager.paths.remoteRegistry, "{invalid")
	current := model{
		manager: manager,
		remoteTopics: []remoteTopic{{
			Slug: "testing", Name: "stale",
		}},
		width:  60,
		height: 10,
	}
	current.tab = claudeTab
	updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyRight})
	current = updated.(model)
	if len(current.remoteTopics) != 0 || !strings.HasPrefix(current.status, "error: ") {
		t.Fatalf("remote cache failure left model %#v", current)
	}
	if strings.Contains(current.View(), "stale") {
		t.Fatalf("remote cache failure rendered stale topics:\n%s", current.View())
	}
}

func TestSkillListHandlesTinyWindowAndMalformedViewportState(t *testing.T) {
	current := model{
		skills:   skillNames(3),
		selected: map[string]bool{},
		cursor:   99,
		offset:   -4,
	}
	updated, _ := current.Update(tea.WindowSizeMsg{Width: 80, Height: 2})
	current = updated.(model)

	if current.cursor != 2 || current.offset != 2 {
		t.Fatalf("cursor, offset = %d, %d; want 2, 2", current.cursor, current.offset)
	}
	for _, skill := range current.skills {
		if strings.Contains(current.View(), skill.Name) {
			t.Fatalf("tiny window rendered list item %q:\n%s", skill.Name, current.View())
		}
	}
}

func TestSkillListIgnoresUnknownInputAndFutureMessages(t *testing.T) {
	current := model{
		skills:   skillNames(5),
		selected: map[string]bool{},
		cursor:   2,
		offset:   1,
		height:   9,
	}
	for _, message := range []tea.Msg{
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("z")},
		struct{ future string }{future: "message"},
	} {
		updated, command := current.Update(message)
		if command != nil {
			t.Fatalf("unknown message %#v returned a command", message)
		}
		got := updated.(model)
		if got.cursor != current.cursor || got.offset != current.offset {
			t.Fatalf("unknown message %#v changed cursor, offset to %d, %d", message, got.cursor, got.offset)
		}
	}
}

func TestFilterInputDynamicallyFiltersInstalledSkillsAndActions(t *testing.T) {
	current := model{
		skills: []discoveredSkill{
			{Name: "alpha", Description: "first", Source: "user"},
			{Name: "beta", Description: "second", Source: "user"},
		},
		selected: map[string]bool{},
		width:    60,
		height:   10,
	}

	updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	current = updated.(model)
	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("SECOND")})
	current = updated.(model)
	view := current.View()
	if !current.filtering || current.itemCount() != 1 ||
		strings.Contains(view, "alpha") || !strings.Contains(view, "beta") ||
		!strings.Contains(view, "Filter: SECOND") {
		t.Fatalf("typing did not dynamically filter to beta:\n%s", view)
	}

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyEnter})
	current = updated.(model)
	if current.filtering {
		t.Fatal("enter did not leave the filter input")
	}

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyEnter})
	current = updated.(model)
	if current.expanded != "beta" {
		t.Fatalf("filtered enter expanded %q, want beta", current.expanded)
	}
}

func TestFilterInputActivatesByMouseAndCanBeCleared(t *testing.T) {
	current := model{
		skills:   skillNames(2),
		selected: map[string]bool{},
		width:    40,
		height:   10,
	}
	updated, _ := current.Update(tea.MouseMsg{
		X:      3,
		Y:      tuiHeaderHeight - 1,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	current = updated.(model)
	if !current.filtering {
		t.Fatal("clicking filter row did not focus the input")
	}

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("missing")})
	current = updated.(model)
	if current.itemCount() != 0 {
		t.Fatalf("typed unmatched filter has %d items, want 0", current.itemCount())
	}
	updated, _ = current.Update(tea.MouseMsg{
		X:      3,
		Y:      tuiHeaderHeight,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	current = updated.(model)
	if current.expanded != "" {
		t.Fatalf("click while editing filter expanded %q", current.expanded)
	}
	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyEnter})
	current = updated.(model)

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	current = updated.(model)
	for range len([]rune("missing")) {
		updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		current = updated.(model)
	}
	if current.itemCount() != 2 {
		t.Fatalf("dynamically cleared filter has %d items, want 2", current.itemCount())
	}
}

func TestSkillsShFilterUsesSharedRequestSearch(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/api/search" ||
			request.URL.Query().Get("q") != "beta testing" {
			t.Errorf("request URL = %s", request.URL.String())
		}
		_ = json.NewEncoder(response).Encode(map[string]any{
			"skills": []map[string]any{{
				"id": "owner/repo/beta", "name": "beta",
				"source": "owner/repo", "installs": 17,
			}},
		})
	}))
	defer server.Close()
	manager := newTestManager(t)
	manager.remote = newRemoteRegistry("")
	manager.remote.baseURL = server.URL
	manager.remote.client = server.Client()
	current := model{
		manager: manager,
		tab:     remoteTab,
		remoteTopics: []remoteTopic{{
			Slug: "testing", Name: "Testing",
			Skills: []remoteSkill{{ID: "owner/repo/unrelated", Name: "beta testing collision"}},
		}},
		filtering: true,
		width:     60,
		height:    10,
	}

	updated, debounce := current.Update(tea.KeyMsg{
		Type: tea.KeyRunes, Runes: []rune("  BETA   TESTING "),
	})
	current = updated.(model)
	if debounce == nil || current.itemCount() != 0 ||
		current.status != "searching skills.sh" {
		t.Fatalf("search did not debounce: %#v", current)
	}
	updated, search := current.Update(registrySearchRequested{
		request: current.registryRequest,
		tab:     remoteTab,
		query:   "beta testing",
	})
	current = updated.(model)
	if search == nil {
		t.Fatal("settled skills.sh query did not start a request")
	}
	updated, _ = current.Update(search())
	current = updated.(model)
	view := current.View()
	if requests != 1 || current.itemCount() != 1 ||
		!strings.Contains(view, "beta") ||
		!strings.Contains(view, "owner/repo • 17 installs") ||
		strings.Contains(view, "collision") {
		t.Fatalf("request search result was not rendered exclusively:\n%s", view)
	}

	updated, command := current.Update(registrySearchDone{
		request: current.registryRequest,
		tab:     skillsMPTab,
		query:   "beta testing",
		skills:  []registrySearchSkill{{ID: "collision", Name: "wrong provider"}},
	})
	got := updated.(model)
	if command != nil || got.registrySkills[0].ID != "owner/repo/beta" {
		t.Fatalf("unrelated provider result replaced skills.sh search: %#v", got)
	}
}

func TestSkillListPrimaryClickSelectsAndTogglesHeader(t *testing.T) {
	current := model{
		skills:   skillNames(4),
		selected: map[string]bool{},
		width:    40,
		height:   12,
	}
	updated, command := current.Update(tea.MouseMsg{
		X:      10,
		Y:      tuiHeaderHeight + 2,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	if command != nil {
		t.Fatal("click returned a command")
	}
	current = updated.(model)
	if current.cursor != 2 || current.expanded != "skill-02" {
		t.Fatalf("cursor, expanded = %d, %q; want 2, skill-02", current.cursor, current.expanded)
	}

	updated, _ = current.Update(tea.MouseMsg{
		X:      10,
		Y:      tuiHeaderHeight + 2,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	current = updated.(model)
	if current.cursor != 2 || current.expanded != "" {
		t.Fatalf("second click left cursor, expanded = %d, %q; want 2, empty", current.cursor, current.expanded)
	}
}

func TestSkillListClickAccountsForExpandedRows(t *testing.T) {
	current := model{
		skills:   skillNames(3),
		selected: map[string]bool{},
		expanded: "skill-00",
		width:    40,
		height:   14,
	}
	current.syncViewport()
	indices := current.localSkillIndices()
	secondHeader := tuiHeaderHeight + len(current.skillLines(indices, 0))

	updated, _ := current.Update(tea.MouseMsg{
		X:      1,
		Y:      secondHeader,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	got := updated.(model)
	if got.cursor != 1 || got.expanded != "skill-01" {
		t.Fatalf("cursor, expanded = %d, %q; want 1, skill-01", got.cursor, got.expanded)
	}
}

func TestSkillListClickIgnoresDetailsChromeAndOtherButtons(t *testing.T) {
	current := model{
		skills:   skillNames(3),
		selected: map[string]bool{},
		cursor:   1,
		expanded: "skill-01",
		width:    40,
		height:   14,
	}
	current.syncViewport()
	for _, message := range []tea.MouseMsg{
		{X: 1, Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft},
		{X: 1, Y: tuiHeaderHeight + 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft},
		{X: 1, Y: current.height - 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft},
		{X: 1, Y: tuiHeaderHeight, Action: tea.MouseActionRelease, Button: tea.MouseButtonLeft},
		{X: 1, Y: tuiHeaderHeight, Action: tea.MouseActionPress, Button: tea.MouseButtonRight},
		{X: current.width, Y: tuiHeaderHeight, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft},
	} {
		updated, command := current.Update(message)
		if command != nil {
			t.Fatalf("ignored mouse message %#v returned a command", message)
		}
		got := updated.(model)
		if got.cursor != current.cursor || got.expanded != current.expanded {
			t.Fatalf(
				"ignored mouse message %#v changed cursor, expanded to %d, %q",
				message,
				got.cursor,
				got.expanded,
			)
		}
	}
}

func TestTUIStylesUseColorWithoutReplacingTextState(t *testing.T) {
	for name, style := range map[string]lipgloss.Style{
		"title":      titleStyle,
		"disclosure": disclosureStyle,
		"disabled":   disabledStyle,
		"inherited":  inheritedStyle,
		"muted":      mutedStyle,
		"path":       pathStyle,
		"success":    statusStyle("enabled alpha"),
		"warning":    statusStyle("updating alpha"),
		"error":      statusStyle("error: failed"),
	} {
		if style.GetForeground() == nil {
			t.Errorf("%s style has no foreground color", name)
		}
	}
	if selectedStyle(lipgloss.NewStyle(), true).GetBackground() == nil {
		t.Fatal("selected style has no background color")
	}
	if inheritedStyle.GetForeground() == disabledStyle.GetForeground() {
		t.Fatal("inherited and disabled markers should use different colors")
	}
	if !pathStyle.GetUnderline() || mutedStyle.GetUnderline() {
		t.Fatal("path value should be underlined independently of its label and indentation")
	}

	current := model{
		skills:   []discoveredSkill{{Name: "alpha", Source: "user"}},
		selected: map[string]bool{},
		width:    30,
		height:   8,
	}
	view := current.View()
	for _, fallback := range []string{"▸", "alpha", "[disabled]", "(user)"} {
		if !strings.Contains(view, fallback) {
			t.Errorf("colored view omitted textual fallback %q:\n%s", fallback, view)
		}
	}
}

func TestStyledPathLinesWrapOnlyPathCharacters(t *testing.T) {
	const width = 20
	lines := styledPathLines("/skills/世界/alpha/SKILL.md", width)
	if len(lines) < 2 {
		t.Fatalf("path did not wrap: %#v", lines)
	}
	for index, line := range lines {
		if lipgloss.Width(line) > width {
			t.Errorf("line %d width = %d, want <= %d: %q", index, lipgloss.Width(line), width, line)
		}
	}
	if !strings.Contains(lines[0], "path  /skills/") {
		t.Fatalf("first path line does not contain label and value: %q", lines[0])
	}
	if strings.Contains(lines[1], "path") {
		t.Fatalf("continuation repeated path label: %q", lines[1])
	}
}

func TestTerminalSafeTextEscapesControlCharacters(t *testing.T) {
	path := "before\x1b\n\u009bafter"
	safe := terminalSafeText(path)
	if safe != `before\x1b\x0a\x9bafter` {
		t.Fatalf("safe path = %q", safe)
	}
	for _, control := range []string{"\x1b", "\n", "\u009b"} {
		if strings.Contains(safe, control) {
			t.Fatalf("safe path retains control character %q: %q", control, safe)
		}
	}

	current := model{
		project: path,
		skills: []discoveredSkill{{
			Name: "alpha", Description: "Alpha.", Path: path, Source: "plugin",
		}},
		selected: map[string]bool{"alpha": true},
		expanded: "alpha",
		width:    40,
		height:   12,
	}
	view := current.View()
	if strings.Contains(view, "\x1b]") || strings.Count(view, `\x1b\x0a\x9b`) != 2 {
		t.Fatalf("view did not safely render filesystem controls: %q", view)
	}
}

func TestEveryRenderedLineFitsNarrowTerminal(t *testing.T) {
	for _, width := range []int{1, 4, 5, 9, 20} {
		current := model{
			project: strings.Repeat("project/", 20),
			skills: []discoveredSkill{{
				Name:        strings.Repeat("n", maxSkillNameLen),
				Description: strings.Repeat("wide 世界 ", 20),
				Path:        "/" + strings.Repeat("long-path/", 20) + "SKILL.md",
				Source:      "plugin",
			}},
			selected: map[string]bool{},
			expanded: strings.Repeat("n", maxSkillNameLen),
			status:   "error: " + strings.Repeat("failed ", 20),
			width:    width,
			height:   30,
		}
		for lineNumber, line := range strings.Split(current.View(), "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("width %d line %d has display width %d: %q", width, lineNumber, got, line)
			}
		}
	}
}

func TestEditDoneRefreshesMetadataByCanonicalPath(t *testing.T) {
	const path = "/skills/alpha/SKILL.md"
	current := model{
		skills: []discoveredSkill{{
			Name: "alpha", Description: "Old.", Path: path, Source: "user",
		}},
		selected: map[string]bool{"alpha": true},
		expanded: "alpha",
		busy:     true,
	}
	updated, _ := current.Update(editDone{
		skill: "alpha",
		path:  path,
		skills: []discoveredSkill{{
			Name: "renamed", Description: "New.", Path: path, Source: "user",
		}},
		selected: map[string]bool{"renamed": true},
	})
	got := updated.(model)
	if got.busy || got.skills[0].Name != "renamed" || got.skills[0].Description != "New." {
		t.Fatalf("edit refresh produced model %#v", got)
	}
	if got.cursor != 0 || got.expanded != "renamed" {
		t.Fatalf("cursor, expanded = %d, %q; want 0, renamed", got.cursor, got.expanded)
	}
}

func TestEditDoneRefreshErrorPreservesMetadata(t *testing.T) {
	current := model{
		skills: []discoveredSkill{{
			Name: "alpha", Description: "Old.", Path: "/skills/alpha/SKILL.md",
		}},
		selected: map[string]bool{"alpha": true},
		expanded: "alpha",
		busy:     true,
	}
	updated, _ := current.Update(editDone{
		skill: "alpha", refreshErr: errors.New("rediscovery failed"),
	})
	got := updated.(model)
	if got.busy || got.skills[0].Description != "Old." || got.expanded != "alpha" {
		t.Fatalf("refresh error changed metadata: %#v", got)
	}
	if got.status != "refresh failed: rediscovery failed" {
		t.Fatalf("status = %q", got.status)
	}
}

func TestRefreshEditedSkillMigratesRenamedSelection(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md")
	writeFile(t, path, skillFile("alpha", "Old.", ""))
	if err := saveLock(project, lock{Skills: map[string]bool{"alpha": true}}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, skillFile("renamed", "New.", ""))

	skills, selected, err := manager.refreshEditedSkill(project, "alpha", path)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "renamed" || skills[0].Description != "New." {
		t.Fatalf("skills = %#v", skills)
	}
	if !selected["renamed"] {
		t.Fatalf("renamed skill is not enabled: %#v", selected)
	}
	if _, exists := selected["alpha"]; exists {
		t.Fatalf("obsolete selection was retained: %#v", selected)
	}

	value, err := loadLock(project)
	if err != nil {
		t.Fatal(err)
	}
	if !value.Skills["renamed"] {
		t.Fatalf("persisted selection = %#v", value.Skills)
	}
	var output strings.Builder
	if err := manager.list(project, &output); err != nil {
		t.Fatalf("list after rename: %v", err)
	}
	if !strings.Contains(output.String(), "<name>renamed</name>") {
		t.Fatalf("list omitted renamed skill:\n%s", output.String())
	}
}

func TestRefreshEditedSkillOverridesInheritedGlobalSelection(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md")
	writeFile(t, path, skillFile("alpha", "Old.", ""))
	if err := saveLock(manager.paths.globalLockDir, lock{
		Skills: map[string]bool{"alpha": true},
	}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, skillFile("renamed", "New.", ""))

	_, selected, err := manager.refreshEditedSkill(project, "alpha", path)
	if err != nil {
		t.Fatal(err)
	}
	if !selected["renamed"] || selected["alpha"] {
		t.Fatalf("selection after inherited rename = %#v", selected)
	}
	assertLock(t, project, map[string]bool{
		"alpha":   false,
		"renamed": true,
	})
	assertLock(t, manager.paths.globalLockDir, map[string]bool{"alpha": true})
}

func TestRefreshEditedSkillPreservesProjectOverrideOfGlobalSelection(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	path := filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md")
	writeFile(t, path, skillFile("alpha", "Old.", ""))
	if err := saveLock(manager.paths.globalLockDir, lock{
		Skills: map[string]bool{"alpha": true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveLock(project, lock{
		Skills: map[string]bool{"alpha": false},
	}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, skillFile("renamed", "New.", ""))

	_, selected, err := manager.refreshEditedSkill(project, "alpha", path)
	if err != nil {
		t.Fatal(err)
	}
	if selected["renamed"] || selected["alpha"] {
		t.Fatalf("selection after overridden rename = %#v", selected)
	}
	assertLock(t, project, map[string]bool{
		"alpha":   false,
		"renamed": false,
	})
	assertLock(t, manager.paths.globalLockDir, map[string]bool{"alpha": true})
}

func TestRefreshEditedSkillMigratesGlobalSelectionInGlobalMode(t *testing.T) {
	manager := newTestManager(t)
	manager.global = true
	project := t.TempDir()
	path := filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md")
	writeFile(t, path, skillFile("alpha", "Old.", ""))
	if err := saveLock(manager.paths.globalLockDir, lock{
		Skills: map[string]bool{"alpha": true},
	}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, skillFile("renamed", "New.", ""))

	_, selected, err := manager.refreshEditedSkill(project, "alpha", path)
	if err != nil {
		t.Fatal(err)
	}
	if !selected["renamed"] {
		t.Fatalf("global selection after rename = %#v", selected)
	}
	if _, exists := selected["alpha"]; exists {
		t.Fatalf("obsolete global selection was retained: %#v", selected)
	}
	assertLock(t, manager.paths.globalLockDir, map[string]bool{"renamed": true})
}

func TestSkillListIgnoresClicksWhileBusy(t *testing.T) {
	current := model{
		skills:   skillNames(2),
		selected: map[string]bool{},
		width:    40,
		height:   10,
		busy:     true,
	}
	updated, command := current.Update(tea.MouseMsg{
		X:      1,
		Y:      tuiHeaderHeight + 1,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	})
	got := updated.(model)
	if command != nil || got.cursor != 0 || got.expanded != "" {
		t.Fatalf("busy click changed model to %#v with command %v", got, command)
	}
}

func TestSkillListResizeKeepsSelectionVisible(t *testing.T) {
	current := model{
		skills:   skillNames(10),
		selected: map[string]bool{},
		cursor:   7,
		width:    80,
		height:   12,
	}
	current.syncViewport()

	updated, _ := current.Update(tea.WindowSizeMsg{Width: 80, Height: 8})
	current = updated.(model)
	if current.offset != 7 {
		t.Fatalf("offset after resize = %d, want 7", current.offset)
	}
	if !strings.Contains(current.View(), "> ▸ skill-07 [disabled]") {
		t.Fatalf("selected skill is not visible after resize:\n%s", current.View())
	}
}

func TestSkillListShowsSourceAndDisabledState(t *testing.T) {
	current := model{
		skills: []discoveredSkill{
			{Name: "disabled", Source: "user"},
			{Name: "enabled", Source: "project"},
			{Name: "future"},
		},
		selected: map[string]bool{"enabled": true},
		width:    40,
		height:   12,
	}

	view := current.View()
	for _, want := range []string{
		"> ▸ enabled                    (project)",
		"  ▸ disabled [disabled]           (user)",
		"  ▸ future [disabled]          (unknown)",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("view does not contain %q:\n%s", want, view)
		}
	}
	if strings.Contains(strings.Split(view, "\n")[3], "[disabled]") {
		t.Fatalf("enabled skill is labeled disabled:\n%s", view)
	}
}

func TestSkillListMarksManualOnlyState(t *testing.T) {
	current := model{
		skills: []discoveredSkill{
			{Name: "manual", DisableModelInvocation: true},
			{Name: "inherited", DisableModelInvocation: true},
			{Name: "disabled", DisableModelInvocation: true},
			{Name: "automatic"},
		},
		selected: map[string]bool{
			"manual":    true,
			"inherited": true,
			"automatic": true,
		},
		globalSelected: map[string]bool{"inherited": true},
		projectSelected: map[string]bool{
			"manual":    true,
			"disabled":  false,
			"automatic": true,
		},
		width:  80,
		height: 14,
	}

	view := current.View()
	for _, want := range []string{
		"manual [manual-only]",
		"inherited [inherited] [manual-only]",
		"disabled [manual-only] [disabled]",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("view does not contain %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "automatic [manual-only]") {
		t.Fatalf("model-invocable skill is marked manual-only:\n%s", view)
	}
}

func TestInstalledSkillsSortEnabledFirst(t *testing.T) {
	current := model{
		skills: []discoveredSkill{
			{Name: "disabled-first"},
			{Name: "enabled-first"},
			{Name: "project-disabled-global-enabled"},
			{Name: "inherited-global-enabled"},
			{Name: "disabled-second"},
			{Name: "enabled-second"},
			{Name: "inherited-global-disabled"},
		},
		selected: map[string]bool{
			"enabled-first":            true,
			"enabled-second":           true,
			"inherited-global-enabled": true,
		},
		globalSelected: map[string]bool{
			"project-disabled-global-enabled": true,
			"inherited-global-enabled":        true,
			"inherited-global-disabled":       false,
		},
		projectSelected: map[string]bool{
			"enabled-first":                   true,
			"enabled-second":                  true,
			"project-disabled-global-enabled": false,
		},
	}

	indices := current.localSkillIndices()
	got := make([]string, len(indices))
	for index, skillIndex := range indices {
		got[index] = current.skills[skillIndex].Name
	}
	want := []string{
		"enabled-first",
		"enabled-second",
		"project-disabled-global-enabled",
		"inherited-global-enabled",
		"disabled-first",
		"disabled-second",
		"inherited-global-disabled",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("installed skill order = %q, want %q", got, want)
	}
}

func TestSkillListMarksOnlyInheritedState(t *testing.T) {
	inherited := model{
		skills:          []discoveredSkill{{Name: "alpha"}},
		selected:        map[string]bool{"alpha": true},
		globalSelected:  map[string]bool{"alpha": true},
		projectSelected: map[string]bool{},
		width:           40,
		height:          12,
	}
	if view := inherited.View(); !strings.Contains(view, "alpha [inherited]") {
		t.Fatalf("inherited skill has no marker:\n%s", view)
	}

	inheritedDisabled := inherited
	inheritedDisabled.selected = map[string]bool{"alpha": false}
	inheritedDisabled.globalSelected = map[string]bool{"alpha": false}
	if view := inheritedDisabled.View(); strings.Contains(view, "[inherited]") ||
		!strings.Contains(view, "alpha [disabled]") {
		t.Fatalf("inherited disabled skill has wrong markers:\n%s", view)
	}

	projectOverride := inherited
	projectOverride.projectSelected = map[string]bool{"alpha": true}
	if view := projectOverride.View(); strings.Contains(view, "[inherited]") {
		t.Fatalf("project override is marked inherited:\n%s", view)
	}

	projectDisabled := inherited
	projectDisabled.selected = map[string]bool{"alpha": false}
	projectDisabled.projectSelected = map[string]bool{"alpha": false}
	if view := projectDisabled.View(); strings.Contains(view, "[inherited]") ||
		!strings.Contains(view, "alpha [disabled]") {
		t.Fatalf("project-disabled global skill has wrong markers:\n%s", view)
	}

	unrelatedGlobalKey := inherited
	unrelatedGlobalKey.selected = map[string]bool{}
	unrelatedGlobalKey.globalSelected = map[string]bool{"alpha-extra": true}
	if view := unrelatedGlobalKey.View(); strings.Contains(view, "[inherited]") {
		t.Fatalf("unrelated global key marked skill inherited:\n%s", view)
	}

	globalMode := inherited
	globalMode.projectSelected = nil
	if view := globalMode.View(); strings.Contains(view, "[inherited]") {
		t.Fatalf("global mode skill is marked inherited:\n%s", view)
	}
}

func TestSkillListExpandsAndCollapsesDetails(t *testing.T) {
	current := model{
		skills: []discoveredSkill{{
			Name:        "alpha",
			Description: "Alpha handles unicode safely: 世界.",
			Path:        "/skills/alpha/SKILL.md",
			Source:      "user",
		}},
		selected: map[string]bool{"alpha": true},
		width:    32,
		height:   14,
	}

	updated, command := current.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil {
		t.Fatal("expanding details returned a command")
	}
	current = updated.(model)
	view := current.View()
	for _, want := range []string{"▾ alpha", "Alpha handles unicode", "safely: 世界.", "path  /skills/alpha/SKILL.md"} {
		if !strings.Contains(view, want) {
			t.Fatalf("expanded view does not contain %q:\n%s", want, view)
		}
	}

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyEnter})
	current = updated.(model)
	view = current.View()
	if current.expanded != "" || strings.Contains(view, "Alpha handles") || strings.Contains(view, "SKILL.md") {
		t.Fatalf("collapsed view retained details:\n%s", view)
	}
}

func TestExpandedSkillParticipatesInViewportScrolling(t *testing.T) {
	current := model{
		skills:   skillNames(5),
		selected: map[string]bool{},
		cursor:   3,
		expanded: "skill-03",
		width:    24,
		height:   11,
	}
	current.syncViewport()

	if current.offset != 3 {
		t.Fatalf("offset = %d, want expanded skill at viewport start", current.offset)
	}
	view := current.View()
	if !strings.Contains(view, "> ▾ skill-03") || strings.Contains(view, "skill-02") {
		t.Fatalf("expanded skill is not visible within viewport:\n%s", view)
	}
}

func skillNames(count int) []discoveredSkill {
	names := make([]discoveredSkill, count)
	for index := range names {
		name := fmt.Sprintf("skill-%02d", index)
		names[index] = discoveredSkill{
			Name:        name,
			Description: "Description for " + name + ".",
			Path:        "/skills/" + name + "/SKILL.md",
			Source:      "user",
		}
	}
	return names
}

func TestEditorTargetsCanonicalSkill(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	skill := filepath.Join(manager.paths.userSkills, "alpha", "SKILL.md")
	writeFile(t, skill, skillFile("alpha", "Alpha.", "before"))
	editor := filepath.Join(t.TempDir(), "editor")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\nprintf edited > \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", editor+" --flag")

	command, err := (model{manager: manager, project: project}).editor("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	assertFile(t, skill, "edited")
}

func TestToggleErrorPreservesSelection(t *testing.T) {
	current := model{
		selected: map[string]bool{"alpha": true},
		busy:     true,
	}

	updated, _ := current.Update(toggleDone{
		skill:   "alpha",
		enabled: false,
		err:     errors.New("write failed"),
	})
	got := updated.(model)
	if !got.selected["alpha"] {
		t.Fatal("toggle error changed the displayed selection")
	}
	if got.busy {
		t.Fatal("toggle error left the model busy")
	}
	if got.status != "error: write failed" {
		t.Fatalf("status = %q", got.status)
	}
}

func TestEditorRejectsNonEditableSkillSource(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeSkill(t, filepath.Join(manager.paths.adminSkills, "admin"), "admin")
	t.Setenv("EDITOR", "editor")

	_, err := (model{manager: manager, project: project}).editor("admin")
	if err == nil || !strings.Contains(err.Error(), "not editable") {
		t.Fatalf("error = %v, want non-editable source error", err)
	}
}

func TestApplyCatalogClearsWhenDiscoveryEmpty(t *testing.T) {
	current := model{
		skills: []discoveredSkill{{Name: "stale", Source: "user"}},
	}
	current.applyCatalog()
	if len(current.skills) != 0 {
		t.Fatalf("empty rediscovery left catalog %#v", current.skills)
	}
}

func TestTUICatalogFiltersAgentPrivateSkills(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeSkill(t, filepath.Join(manager.paths.userSkills, "common"), "common")
	writeSkill(t, filepath.Join(project, ".agents", "skills", "local"), "local")
	writeSkill(t, filepath.Join(project, ".claude", "skills", "project-claude"), "project-claude")
	writeSkill(t, filepath.Join(project, ".grok", "skills", "project-grok"), "project-grok")
	writeSkill(t, filepath.Join(project, ".codex", "skills", "project-codex"), "project-codex")
	writeSkill(t, filepath.Join(manager.paths.codexHome, "skills", "codex-only"), "codex-only")
	writeSkill(
		t,
		filepath.Join(manager.paths.codexHome, "plugins", "cache", "publisher", "plugin", "hash", "skills", "plugin-only"),
		"plugin-only",
	)
	writeSkill(t, filepath.Join(manager.paths.codexHome, "skills", ".system", "builtin"), "builtin")
	writeSkill(t, filepath.Join(manager.paths.adminSkills, "admin-only"), "admin-only")

	current, err := newModel(manager, project)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		tab         int
		codexSubtab int
		want        []string
		omit        []string
	}{
		{
			name: "installed hides all private skills",
			tab:  localTab,
			want: []string{"common", "local"},
			omit: []string{"admin-only", "builtin", "codex-only", "plugin-only", "project-claude", "project-codex", "project-grok"},
		},
		{
			name:        "codex user subtab",
			tab:         codexTab,
			codexSubtab: codexUserSubtab,
			want:        []string{"codex-only", "project-codex"},
			omit:        []string{"admin-only", "builtin", "common", "local", "plugin-only", "project-claude", "project-grok"},
		},
		{
			name:        "codex plugin subtab",
			tab:         codexTab,
			codexSubtab: codexPluginSubtab,
			want:        []string{"plugin-only"},
			omit:        []string{"admin-only", "builtin", "codex-only", "common", "local", "project-claude", "project-codex", "project-grok"},
		},
		{
			name:        "codex builtin subtab",
			tab:         codexTab,
			codexSubtab: codexBuiltinSubtab,
			want:        []string{"builtin"},
			omit:        []string{"admin-only", "codex-only", "common", "local", "plugin-only", "project-claude", "project-codex", "project-grok"},
		},
		{
			name:        "codex system subtab",
			tab:         codexTab,
			codexSubtab: codexSystemSubtab,
			want:        []string{"admin-only"},
			omit:        []string{"builtin", "codex-only", "common", "local", "plugin-only", "project-claude", "project-codex", "project-grok"},
		},
		{
			name: "claude keeps Claude-private skills",
			tab:  claudeTab,
			want: []string{"project-claude"},
			omit: []string{"admin-only", "builtin", "codex-only", "common", "local", "plugin-only", "project-codex", "project-grok"},
		},
		{
			name: "grok keeps Grok-private skills",
			tab:  grokTab,
			want: []string{"project-grok"},
			omit: []string{"admin-only", "builtin", "codex-only", "common", "local", "plugin-only", "project-claude", "project-codex"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := current
			got.tab = tt.tab
			got.codexSubtab = tt.codexSubtab
			got.applyCatalog()
			names := make([]string, 0, len(got.skills))
			for _, skill := range got.skills {
				names = append(names, skill.Name)
			}
			for _, name := range tt.want {
				if !slices.Contains(names, name) {
					t.Errorf("catalog omitted %q: %q", name, names)
				}
			}
			for _, name := range tt.omit {
				if slices.Contains(names, name) {
					t.Errorf("catalog included private skill %q: %q", name, names)
				}
			}
		})
	}
}

func TestCodexSubtabsCycleWithBrackets(t *testing.T) {
	manager := newTestManager(t)
	project := t.TempDir()
	writeSkill(t, filepath.Join(manager.paths.codexHome, "skills", "user-skill"), "user-skill")
	writeSkill(
		t,
		filepath.Join(manager.paths.codexHome, "plugins", "cache", "publisher", "plugin", "hash", "skills", "plugin-skill"),
		"plugin-skill",
	)
	current, err := newModel(manager, project)
	if err != nil {
		t.Fatal(err)
	}
	current.width = 80
	current.height = 16
	updated, _ := current.Update(tea.KeyMsg{Type: tea.KeyRight})
	current = updated.(model)
	if current.tab != codexTab {
		t.Fatalf("tab = %d, want Codex", current.tab)
	}
	view := current.View()
	if !strings.Contains(view, "[User]") || !strings.Contains(view, "user-skill") ||
		strings.Contains(view, "plugin-skill") {
		t.Fatalf("user subtab view:\n%s", view)
	}

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{']'}})
	current = updated.(model)
	if current.codexSubtab != codexPluginSubtab {
		t.Fatalf("subtab = %d, want Plugin", current.codexSubtab)
	}
	view = current.View()
	if !strings.Contains(view, "[Plugin]") || !strings.Contains(view, "plugin-skill") ||
		strings.Contains(view, "user-skill") {
		t.Fatalf("plugin subtab view:\n%s", view)
	}

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'['}})
	current = updated.(model)
	if current.codexSubtab != codexUserSubtab {
		t.Fatalf("subtab = %d, want User", current.codexSubtab)
	}

	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'['}})
	current = updated.(model)
	if current.codexSubtab != codexSystemSubtab {
		t.Fatalf("wrap back = %d, want System", current.codexSubtab)
	}
	updated, _ = current.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{']'}})
	current = updated.(model)
	if current.codexSubtab != codexUserSubtab {
		t.Fatalf("wrap forward = %d, want User", current.codexSubtab)
	}
}
